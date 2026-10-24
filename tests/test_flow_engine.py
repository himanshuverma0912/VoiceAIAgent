"""End-to-end tests of the flow engine (flow_engine/runtime.py).

Each test runs a whole conversation in LiveKit's text mode with a scripted LLM
(tests/scripted_llm.py), so no network, API keys or phone call are needed. The
same engine code runs both flows in flows/, which is the point: nothing in the
engine is specific to one project.
"""

import asyncio
from types import SimpleNamespace

import pytest
from livekit.agents import AgentSession, function_tool
from livekit.agents.beta.workflows import dtmf_inputs
from livekit.agents.voice.agent_activity import ActivityClosedError

from flow_engine.runtime import NOT_PROVIDED, CallState, start_agent
from flow_engine.spec import load_flow
from scripted_llm import ScriptedLLM, call

CALLER = "+919990187773"


# ── helpers ───────────────────────────────────────────────────────────────────


class Harness:
    def __init__(self, flow_file: str) -> None:
        self.ended: list[str] = []
        self.state = CallState(
            flow=load_flow(f"flows/{flow_file}"), caller_number=CALLER, end_call=self._fake_end_call
        )
        self.llm = ScriptedLLM()

    def _fake_end_call(self, final_words: str, extra: str):
        # Same name and behaviour as the real EndCallTool (its result tells the LLM
        # what to say last), but it records the hang-up instead of deleting a room.
        @function_tool(name="end_call", description=f"End the call. {extra}")
        async def end_call() -> str:
            self.ended.append(final_words)
            return final_words

        return [end_call]

    def steps(self) -> list[str]:
        """Steps the LLM was asked to speak in, in order, without repeats."""
        out: list[str] = []
        for r in self.llm.requests:
            if r["step"] != "task" and (not out or out[-1] != r["step"]):
                out.append(r["step"])
        return out

    def last_instructions(self, step: str) -> str:
        return [r for r in self.llm.requests if r["step"] == step][-1]["instructions"]


async def settle(session: AgentSession) -> None:
    """Wait until the session is idle and no handoff is still in progress."""
    for _ in range(300):
        if session._is_closing():  # the call has ended (e.g. end_call ran): nothing to wait for
            await asyncio.sleep(0.3)
            return
        try:
            await session.wait_for_idle()
        except ActivityClosedError:  # an activity is mid-handoff; try again
            await asyncio.sleep(0.02)
            continue
        await asyncio.sleep(0.05)
        task = session._update_activity_atask
        if task is None or task.done():
            try:
                await session.wait_for_idle()
                return
            except ActivityClosedError:
                pass
        await asyncio.sleep(0.02)
    raise TimeoutError("session never settled")


async def converse(h: Harness, messages: list[str]) -> list:
    """Run the conversation, close the session, and return the chat history items."""
    session = AgentSession(llm=h.llm)
    try:
        await session.start(start_agent(h.state))
        await settle(session)
        for msg in messages:
            if session._is_closing():
                break
            await asyncio.wait_for(session.run(user_input=msg), 15)
            await asyncio.wait_for(settle(session), 20)
        return list(session.history.items)
    finally:
        await session.aclose()


@pytest.fixture(autouse=True)
def fake_dtmf_room(monkeypatch):
    """The keypad-digits task listens on the call's LiveKit room; give it a stand-in."""
    room = SimpleNamespace(on=lambda *a, **k: None, off=lambda *a, **k: None)
    monkeypatch.setattr(dtmf_inputs, "get_job_context", lambda: SimpleNamespace(room=room))


# ── complaint helpline ────────────────────────────────────────────────────────


async def test_complaint_flow_end_to_end():
    h = Harness("complaint-helpline.json")
    history = await converse(
        h,
        [
            "Mere area mein street light kharab hai",
            call("go_to_collect_details", issue_category="electricity", issue_summary="Street light not working"),
            call("update_name", first_name="Ravi", last_name="Kumar"),
            call("update_phone_number", phone_number=CALLER),
            call("record_city", value="Noida"),
            call("record_locality", value="Sector 62"),
            call("go_to_closing"),  # the caller says they have nothing more to ask
        ],
    )

    assert h.state.path == ["listen_issue", "collect_details", "register", "closing"]
    assert h.state.values == {
        "issue_category": "electricity",
        "issue_summary": "Street light not working",
        "full_name": "Ravi Kumar",
        "mobile_number": CALLER,
        "city": "Noida",
        "locality": "Sector 62",
    }
    # Entering the end node hangs up on its own: no further caller turn was needed.
    assert len(h.ended) == 1
    assert "will be forwarded to the concerned department" in h.ended[0]
    assert "say goodbye" in h.ended[0]
    # ...and the final words were spoken after the hang-up tool ran.
    assert history[-1].type == "message" and history[-1].role == "assistant"

    # The greeting is the first thing said, word for word.
    first_reply = next(i for i in history if i.type == "message" and i.role == "assistant")
    assert first_reply.text_content == h.state.flow.greeting

    # listen_issue waits for the caller; register speaks first with the values filled in.
    register = h.last_instructions("register")
    for expected in ("Street light not working", "Ravi Kumar", "Sector 62", "Noida", "IST"):
        assert expected in register
    assert "{{" not in register
    assert "Information already collected" in register

    # Persona and guardrails are part of every node's instructions.
    assert "Do not give legal advice." in register
    assert "Complaint Department Helpdesk" in h.last_instructions("closing")


async def test_phone_question_uses_callers_number():
    h = Harness("complaint-helpline.json")
    await converse(
        h,
        [
            "Paani nahi aa raha",
            call("go_to_collect_details", issue_category="water", issue_summary="No water supply"),
            call("update_name", first_name="Asha", last_name="Verma"),
        ],
    )
    phone_task = [r for r in h.llm.requests if "update_phone_number" in r["tools"]][-1]
    assert f"The caller is calling from {CALLER}" in phone_task["instructions"]


async def test_only_this_steps_tools_are_offered():
    h = Harness("complaint-helpline.json")
    await converse(h, ["hello"])
    listen = [r for r in h.llm.requests if r["step"] == "listen_issue"][-1]
    assert listen["tools"] == ["end_call", "go_to_collect_details"]

    # Asking for a tool from a later step does nothing harmful: it simply isn't offered here.
    other = Harness("complaint-helpline.json")
    await converse(other, [call("go_to_closing")])
    assert other.state.path == ["listen_issue"]


# ── EMI payment issue: a different flow, same engine ──────────────────────────


async def test_emi_flow_double_debit_branch():
    h = Harness("emi-payment-issue.json")
    await converse(
        h,
        [
            call("update_dob", year=1990, month=5, day=14),
            call("record_inputs", inputs=["4", "3", "2", "1"]),
            "EMI do baar kat gaya",
            call("go_to_double_debit", debit_date="5 October"),
            call("go_to_closing"),
        ],
    )
    assert h.state.path == ["verify_customer", "identify_issue", "double_debit", "closing"]
    assert h.state.values["date_of_birth"] == "1990-05-14"
    assert h.state.values["loan_last4"] == "4321"
    assert h.state.values["debit_date"] == "5 October"
    assert "5 October" in h.last_instructions("double_debit")
    assert h.ended and "Demo Finance" in h.ended[0]


async def test_emi_flow_callback_branch_uses_caller_number():
    h = Harness("emi-payment-issue.json")
    await converse(
        h,
        [
            call("update_dob", year=1985, month=1, day=2),
            call("record_inputs", inputs=["9", "9", "0", "0"]),
            call("go_to_callback", issue_summary="Wants to change EMI date"),
        ],
    )
    assert h.state.path[-1] == "callback"
    callback = h.last_instructions("callback")
    assert "Wants to change EMI date" in callback
    assert CALLER in callback
    # Values from the other branches were never captured, so they aren't invented.
    assert "debit_date" not in h.state.values and "month" not in h.state.values


async def test_branching_tools_offered_at_identify_issue():
    h = Harness("emi-payment-issue.json")
    await converse(h, [call("update_dob", year=1990, month=5, day=14), call("record_inputs", inputs=["1", "2", "3", "4"])])
    identify = [r for r in h.llm.requests if r["step"] == "identify_issue"][-1]
    assert identify["tools"] == ["end_call", "go_to_callback", "go_to_double_debit", "go_to_missed_emi"]


# ── small units ───────────────────────────────────────────────────────────────


def test_render_fills_known_and_marks_missing():
    state = CallState(flow=load_flow("flows/complaint-helpline.json"), caller_number=CALLER)
    state.values["city"] = "Noida"
    text = state.render("City {{city}}, caller {{caller_number}}, name {{full_name}}")
    assert text == f"City Noida, caller {CALLER}, name {NOT_PROVIDED}"


def test_transition_tool_schema_comes_from_capture():
    state = CallState(flow=load_flow("flows/complaint-helpline.json"))
    agent = start_agent(state)
    tool = next(t for t in agent.tools if getattr(t, "id", "") == "go_to_collect_details")
    params = tool.info.raw_schema["parameters"]
    assert params["required"] == ["issue_category", "issue_summary"]
    assert params["properties"]["issue_category"]["enum"] == [
        "electricity",
        "water",
        "road_infrastructure",
        "society_rwa",
        "other",
    ]


async def test_end_node_hangs_up_without_waiting_for_the_caller():
    """Regression test: the call used to stay open after the goodbye, because hanging
    up waited for the caller to speak again (callers just wait for the line to drop)."""
    h = Harness("emi-payment-issue.json")
    history = await converse(
        h,
        [
            call("update_dob", year=1990, month=5, day=14),
            call("record_inputs", inputs=["4", "3", "2", "1"]),
            call("go_to_callback", issue_summary="Wants to change EMI date"),
            call("go_to_closing", callback_number=CALLER),
            # no further caller message
        ],
    )
    assert h.state.path[-1] == "closing"
    assert len(h.ended) == 1 and "Demo Finance" in h.ended[0]
    calls = [i for i in history if i.type == "function_call"]
    assert calls[-1].name == "end_call"


async def test_caller_hanging_up_mid_collect_does_not_hand_off(caplog):
    """Regression test: on a real call the caller hung up during collect_details, and
    the engine logged a misleading 'flow transition' to the next step."""
    import logging

    caplog.set_level(logging.INFO, logger="flow_engine")
    h = Harness("complaint-helpline.json")
    session = AgentSession(llm=h.llm)
    try:
        await session.start(start_agent(h.state))
        await settle(session)
        for msg in [
            "Street light kharab hai",
            call("go_to_collect_details", issue_category="electricity", issue_summary="Street light not working"),
            call("update_name", first_name="Ravi", last_name="Kumar"),
        ]:
            await asyncio.wait_for(session.run(user_input=msg), 15)
            await asyncio.wait_for(settle(session), 20)
    finally:
        await session.aclose()  # the caller hangs up while the phone number is being asked
    await asyncio.sleep(0.2)

    assert h.state.path == ["listen_issue", "collect_details"]
    messages = [r.getMessage() for r in caplog.records if r.name == "flow_engine"]
    assert "flow call ended" in messages
    assert not any(
        r.getMessage() == "flow transition" and getattr(r, "from", None) == "collect_details"
        for r in caplog.records
    )
    # Per-field logs show exactly where the call was.
    fields = [(r.getMessage(), getattr(r, "field", None)) for r in caplog.records if "field" in r.getMessage()]
    assert ("flow field asked", "full_name") in fields
    assert ("flow field collected", "full_name") in fields
    assert ("flow field asked", "mobile_number") in fields


async def test_caller_can_end_call_in_the_middle_of_a_question():
    """Regression test: on a real call the caller said "my issue got resolved" while being
    asked their name, and the agent kept asking because end_call wasn't offered there."""
    h = Harness("complaint-helpline.json")
    await converse(
        h,
        [
            "Street light kharab hai",
            call("go_to_collect_details", issue_category="electricity", issue_summary="Street light not working"),
            call("end_call"),  # "actually my issue got resolved"
        ],
    )
    assert h.ended and "Acknowledge what the caller said" in h.ended[0]
    name_question = [r for r in h.llm.requests if "update_name" in r["tools"]][0]
    assert "end_call" in name_question["tools"]
    assert "no longer need help" in name_question["instructions"]


async def test_every_kind_of_question_offers_end_call():
    # Custom field (complaint: city) and keypad digits (EMI: last 4 digits).
    h = Harness("complaint-helpline.json")
    await converse(
        h,
        [
            "x",
            call("go_to_collect_details", issue_category="other", issue_summary="x"),
            call("update_name", first_name="A", last_name="B"),
            call("update_phone_number", phone_number=CALLER),
        ],
    )
    city = [r for r in h.llm.requests if "record_city" in r["tools"]][0]
    assert "end_call" in city["tools"] and "no longer need help" in city["instructions"]

    e = Harness("emi-payment-issue.json")
    await converse(e, [call("update_dob", year=1990, month=5, day=14)])
    digits = [r for r in e.llm.requests if "record_inputs" in r["tools"]][0]
    assert "end_call" in digits["tools"] and "no longer need help" in digits["instructions"]


async def test_real_end_call_tool_hangs_up_from_inside_a_question(monkeypatch):
    """Uses LiveKit's real EndCallTool (not the stand-in) with a stand-in job, to check
    the session really shuts down and the room is deleted when ending mid-question."""
    from livekit.agents.beta.tools import end_call as end_call_module

    events: list[str] = []

    class FakeJob:
        def add_shutdown_callback(self, cb):
            events.append("room will be deleted")

        def shutdown(self, reason: str = ""):
            events.append("job shutdown")

        async def delete_room(self, *a, **k):
            events.append("room deleted")

    monkeypatch.setattr(end_call_module, "get_job_context", lambda: FakeJob())
    h = Harness("complaint-helpline.json")
    from flow_engine.runtime import default_end_call

    h.state.end_call = default_end_call  # the real EndCallTool
    await converse(
        h,
        [
            "Street light kharab hai",
            call("go_to_collect_details", issue_category="electricity", issue_summary="x"),
            call("end_call"),
        ],
    )
    assert "room will be deleted" in events and "job shutdown" in events
    assert h.state.path == ["listen_issue", "collect_details"]
