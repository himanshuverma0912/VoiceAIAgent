"""Side listeners (flow_engine/listeners.py): what they hear, and whole calls.

The call tests feed the caller's turns the way speech does (a live transcript,
then the end of the turn), because listeners act at the end of a spoken turn,
which LiveKit's text mode skips.
"""

import asyncio
import json

import pytest
from livekit.agents import AgentSession, llm
from livekit.agents.types import DEFAULT_API_CONNECT_OPTIONS
from livekit.agents.voice.audio_recognition import _EndOfTurnInfo, _EndOfTurnMetrics
from livekit.agents.voice.events import UserInputTranscribedEvent

from flow_engine import listeners as ls
from flow_engine.calllog import CallRecorder
from flow_engine.runtime import start_agent
from flow_engine.spec import FlowError, load_flow, parse_flow
from scripted_llm import call
from test_flow_engine import Harness, fake_dtmf_room, settle  # noqa: F401 (the fixture is autouse)

DOB = call("update_dob", year=1990, month=5, day=14)
DIGITS = call("record_inputs", inputs=["4", "3", "2", "1"])


# ── what they hear ────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "said",
    [
        "my OTP is 4 5 6 7",
        "otp hai 456789",
        "the O.T.P. is 1234",
        "ओटीपी है चार पांच छह सात",
        "PIN is one two three four",
        "cvv 123",
        "card number 4111 1111 1111 1111",
        "4111-1111-1111-1111",
        "mera one time password 5 5 2 1 hai",
    ],
)
def test_secrets_are_recognised(said):
    assert ls.secret_in(said)


@pytest.mark.parametrize(
    "said",
    [
        "my pin code is 560001",
        "pincode 110001",
        "my loan ends with 8765",
        "I was born on 2 November 1985",
        "I didn't get any OTP",
        "what is a PIN",
        "my number is 98765 43210",  # 10 digits: a phone number, not a card
        "I paid 12,990 rupees",
    ],
)
def test_ordinary_answers_are_not_secrets(said):
    assert not ls.secret_in(said)


def test_requests_to_the_llm_never_carry_a_secret():
    ctx = llm.ChatContext()
    ctx.add_message(role="system", content="You help callers.")
    ctx.add_message(role="user", content="my OTP is 4 5 6 7")
    ctx.add_message(role="user", content="my loan ends with 8765")
    masked = ls.mask_chat_ctx(ctx)
    texts = [i.text_content for i in masked.items]
    assert texts == ["You help callers.", ls.SECRET_REMOVED, "my loan ends with 8765"]
    assert ctx.items[1].text_content == "my OTP is 4 5 6 7"  # the original isn't changed
    plain = llm.ChatContext()
    plain.add_message(role="user", content="hello")
    assert ls.mask_chat_ctx(plain) is plain  # nothing to mask: no copy


def test_keywords_match_whole_words_in_any_case():
    words = ["fraud", "without my OTP", "धोखाधड़ी"]
    assert ls.keyword_in(words, "This is FRAUD!")
    assert ls.keyword_in(words, "someone took money without my O.T.P? no, without my otp")
    assert ls.keyword_in(words, "यह धोखाधड़ी है")
    assert not ls.keyword_in(words, "fraudulent")  # whole words only
    assert not ls.keyword_in(words, "with my OTP")


def test_promise_answers_are_read_carefully():
    assert ls._json_object('Sure: {"promise": true, "date": "2026-11-10", "amount": 12990}') == {
        "promise": True,
        "date": "2026-11-10",
        "amount": 12990,
    }
    assert ls._json_object("no json here") is None
    assert ls._amount("₹12,990") == 12990
    assert ls._amount("12990.50") == 12990.5
    assert ls._amount(0) is None and ls._amount(True) is None and ls._amount("soon") is None


# ── the flow format ───────────────────────────────────────────────────────────


def emi_flow(*listeners: dict) -> dict:
    data = json.load(open("flows/emi-payment-issue.json"))
    data["settings"]["listeners"] = list(listeners)
    return data


FRAUD = {"kind": "keyword_watch", "name": "fraud_watch", "words": ["fraud", "without my OTP"], "then": "go_to", "to": "callback"}
GUARD = {"kind": "secret_guard", "name": "secret_guard"}
PROMISE = {"kind": "promise_to_pay", "name": "promise_to_pay", "steps": ["missed_emi"]}


def test_a_promise_defines_variables_the_flow_can_use():
    data = emi_flow(PROMISE)
    data["nodes"]["closing"]["instructions"] += " Remind them they promised to pay on {{promised_date}}."
    parse_flow(data)  # no "unknown variable"
    data["settings"]["resume"] = {"enabled": True, "keep": ["promised_date"]}
    parse_flow(data)


def test_listener_mistakes_are_reported():
    with pytest.raises(FlowError) as e:
        parse_flow(emi_flow({**FRAUD, "to": "nowhere"}, {**FRAUD, "then": "say"}, {**GUARD, "steps": ["closing", "closing"]}))
    assert e.value.problems == [
        "settings.listeners[0].to: 'nowhere' is not a node",
        "settings.listeners[1].name: another listener is already called 'fraud_watch'",
        "settings.listeners[1].to: only used when 'then' is 'go_to'",
        "settings.listeners[1].say: write the line the agent should say",
        "settings.listeners[2].steps[1]: 'closing' is already in the list",
    ]


# ── whole calls ───────────────────────────────────────────────────────────────


class Judge(llm.LLM):
    """The side LLM for promise checks: answers from a table of what was said."""

    def __init__(self, answers: dict[str, dict]) -> None:
        super().__init__()
        self.answers = answers
        self.heard: list[str] = []

    @property
    def model(self) -> str:
        return "judge"

    def chat(self, *, chat_ctx, tools=None, conn_options=DEFAULT_API_CONNECT_OPTIONS, **kwargs):
        return _JudgeStream(self, chat_ctx=chat_ctx, tools=tools or [], conn_options=conn_options)


class _JudgeStream(llm.LLMStream):
    async def _run(self) -> None:
        said = self._chat_ctx.items[-1].text_content or ""
        self._llm.heard.append(said)
        answer = self._llm.answers.get(said, {"promise": False})
        self._event_ch.send_nowait(
            llm.ChatChunk(id="j", delta=llm.ChoiceDelta(role="assistant", content=json.dumps(answer)))
        )


class Call:
    """A call where the caller speaks (see spoken())."""

    def __init__(self, *listeners: dict, judge: Judge | None = None) -> None:
        self.h = Harness("emi-payment-issue.json")
        self.h.state.flow = parse_flow(emi_flow(*listeners))
        self.reports: list[tuple] = []
        self.hung_up = False
        self.h.state.on_listener = lambda *a: self.reports.append(a)
        self.h.state.side_llm = judge

        async def hang_up() -> None:
            self.hung_up = True

        self.h.state.hang_up = hang_up
        self.session = AgentSession(llm=self.h.llm)

    async def __aenter__(self):
        await self.session.start(start_agent(self.h.state))
        self.h.state.listeners.attach(self.session)
        await settle(self.session)
        return self

    async def __aexit__(self, *exc):
        await self.h.state.listeners.aclose()
        await self.session.aclose()

    async def typed(self, *messages: str) -> None:
        """Turns that need no listener (tool calls in the scripted LLM)."""
        for msg in messages:
            await asyncio.wait_for(self.session.run(user_input=msg), 15)
            await asyncio.wait_for(settle(self.session), 20)

    async def spoken(self, text: str, *, interim: str | None = None) -> None:
        if interim:
            self.session._user_input_transcribed(UserInputTranscribedEvent(transcript=interim, is_final=False))
            await asyncio.sleep(0)
        self.session._user_input_transcribed(UserInputTranscribedEvent(transcript=text, is_final=True))
        self.session._activity.on_end_of_turn(
            _EndOfTurnInfo(
                skip_reply=False,
                new_transcript=text,
                transcript_confidence=1.0,
                metrics=_EndOfTurnMetrics(None, None, None, None),
            )
        )
        await asyncio.sleep(0.05)
        await asyncio.wait_for(settle(self.session), 20)
        if self.h.state.listeners._tasks:  # side checks (promise to pay)
            await asyncio.wait(self.h.state.listeners._tasks, timeout=5)

    def history(self) -> list[tuple[str, str]]:
        return [(i.role, i.text_content or "") for i in self.session.history.items if i.type == "message"]

    def llm_saw(self) -> str:
        return "\n".join(r["instructions"] for r in self.h.llm.requests)


async def test_fraud_words_take_the_caller_to_the_chosen_step():
    async with Call(FRAUD) as c:
        await c.typed(DOB, DIGITS)
        await c.spoken("someone took money without my OTP")
        assert c.h.state.path == ["verify_customer", "identify_issue", "callback"]
        assert c.reports == [("fraud_watch", "keyword_watch", "go_to", "identify_issue")]
        # The next step heard what the caller said, and answered it.
        assert ("user", "someone took money without my OTP") in c.history()
        assert c.h.steps()[-1] == "callback"
        # "once": the second time it's just part of the conversation.
        await c.spoken("this is fraud")
        assert len(c.reports) == 1 and c.h.state.path[-1] == "callback"


async def test_fraud_words_work_in_the_middle_of_a_question():
    # verify_customer asks for the date of birth with LiveKit's own question task.
    async with Call(FRAUD) as c:
        await c.spoken("I think this is fraud")
        assert c.h.state.path == ["verify_customer", "callback"]
        assert c.reports == [("fraud_watch", "keyword_watch", "go_to", "verify_customer")]
        await c.typed(call("go_to_closing"))
        assert c.h.state.path == ["verify_customer", "callback", "closing"]
        assert c.h.ended


async def test_a_secret_never_reaches_the_ai_or_the_history():
    async with Call(GUARD) as c:
        await c.typed(DOB, DIGITS)
        before = len(c.h.llm.requests)
        await c.spoken("my OTP is 4 5 6 7", interim="my OTP is 4 5 6")
        said = [t for role, t in c.history() if role == "assistant"]
        assert said.count(ls.SECRET_WARNING[1]) == 1  # warned once, in Hindi (the flow's language)
        assert all("4 5 6 7" not in t for _, t in c.history())
        assert "4 5 6 7" not in c.llm_saw()
        # After the warning the AI carries on with the step, told what happened.
        assert len(c.h.llm.requests) == before + 1
        assert ls.SECRET_FOLLOW_UP in c.h.llm.requests[-1]["instructions"]
        assert c.reports == [("secret_guard", "secret_guard", "warned", "identify_issue")]
        # Ordinary turns go on as usual.
        await c.spoken("my EMI bounced")
        assert ("user", "my EMI bounced") in c.history()


async def test_a_secret_said_at_the_end_of_a_turn_is_caught_too():
    async with Call({**GUARD, "say": "Please don't tell me codes."}) as c:
        await c.typed(DOB, DIGITS)
        await c.spoken("card 4111 1111 1111 1111")  # no live transcript before the end
        assert ("assistant", "Please don't tell me codes.") in c.history()
        assert all("4111" not in t for _, t in c.history())


async def test_a_promise_to_pay_is_kept_without_delaying_the_reply():
    judge = Judge({"I'll pay on the 10th": {"promise": True, "date": "2026-11-10", "amount": 12990}})
    async with Call(PROMISE, judge=judge) as c:
        await c.typed(DOB, DIGITS)
        await c.spoken("I'll pay on the 10th")  # identify_issue: not one of its steps
        assert judge.heard == [] and "promised_date" not in c.h.state.values
        await c.typed(call("go_to_missed_emi"))
        await c.spoken("I'll pay on the 10th")
        assert c.h.state.values["promised_date"] == "2026-11-10"
        assert c.h.state.values["promised_amount"] == 12990
        assert c.reports == [("promise_to_pay", "promise_to_pay", "saved", "missed_emi")]
        await c.spoken("ok thanks")  # not a promise: nothing changes
        assert c.reports == [("promise_to_pay", "promise_to_pay", "saved", "missed_emi")]
        # The next step knows the promise.
        await c.typed(call("go_to_closing"))
        assert "promised_date: 2026-11-10" in c.h.last_instructions("closing")


async def test_say_flag_and_end_call():
    rude = {"kind": "keyword_watch", "name": "abuse", "words": ["idiot"], "then": "say", "say": "Please keep it polite.", "once": False}
    human = {"kind": "keyword_watch", "name": "wants_human", "words": ["human"], "then": "flag"}
    stop = {"kind": "keyword_watch", "name": "do_not_call", "words": ["stop calling"], "then": "end_call"}
    async with Call(rude, human, stop) as c:
        await c.typed(DOB, DIGITS)
        await c.spoken("you idiot")
        await c.spoken("idiot again")
        assert [t for role, t in c.history() if role == "assistant"].count("Please keep it polite.") == 2
        assert ("user", "you idiot") in c.history()  # kept: the agent knows what was said
        await c.spoken("can I talk to a human")  # flagged; the agent answers as usual
        assert c.h.llm.requests[-1]["step"] == "identify_issue"
        await c.spoken("please stop calling me")
        assert c.hung_up
        assert ("assistant", ls.GOODBYE[1]) in c.history()
        assert [r[2] for r in c.reports] == ["say", "say", "flag", "end_call"]


async def test_no_listeners_change_nothing():
    async with Call() as c:
        await c.typed(DOB, DIGITS)
        await c.spoken("my OTP is 4567, this is fraud")
        assert ("user", "my OTP is 4567, this is fraud") in c.history()
        assert c.reports == [] and c.h.state.path[-1] == "identify_issue"


def test_call_record_counts_listeners_only_when_one_acted():
    rec = CallRecorder(call_id="AJ_1", channel="phone", flow_id="f", flow_version=1, flow_source="config-service", end_node_ids=set())
    assert "listeners" not in rec.record()
    rec.listener("fraud_watch", "keyword_watch", "go_to", "identify_issue")
    assert rec.record()["listeners"] == [
        {"listener": "fraud_watch", "kind": "keyword_watch", "action": "go_to", "node": "identify_issue"}
    ]


def test_example_flow_still_loads():
    load_flow("flows/emi-payment-issue.json")
