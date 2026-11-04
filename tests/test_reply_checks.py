"""Reply checks (flow_engine/reply_checks.py): each check on its own, the
streaming wrapper, and a whole credit card call where the LLM gets an amount
wrong, reads out a full card number and promises a waiver."""

import json
from decimal import Decimal
from pathlib import Path

from livekit.agents import llm

from flow_engine import reply_checks as rc
from flow_engine.calllog import CallRecorder
from flow_engine.runtime import CallState
from flow_engine.spec import REPLY_CHECKS, parse_flow
from scripted_llm import call
from test_flow_engine import converse, fake_dtmf_room  # noqa: F401  (the keypad task needs a room)
from test_tools import credit_card_harness

ROOT = Path(__file__).resolve().parent.parent
D = Decimal


def flow_with(checks: list[str], line: str = "", language: str | None = None, steps: dict | None = None):
    data = json.loads((ROOT / "flows" / "credit-card-helpline.json").read_text())
    data["settings"]["reply_checks"] = checks
    if line:
        data["settings"]["reply_check_line"] = line
    if language:
        data["settings"]["language"] = language
    for node, extra in (steps or {}).items():
        data["nodes"][node]["reply_checks"] = extra
    return parse_flow(data)


def ctx(checks=("amounts_from_tools",), **kw) -> rc.CheckContext:
    return rc.CheckContext(CallState(flow=flow_with(list(checks), **kw)), "account_help")


# ── pieces ────────────────────────────────────────────────────────────────────


def test_every_check_the_format_names_exists():
    assert set(REPLY_CHECKS) == set(rc.CHECKS)


def test_sentences_split_at_their_end_but_not_after_rs():
    done, rest = rc.split_sentences("Your due is Rs. 922.54 today. Pay by 15 Nov! And the")
    assert done == ["Your due is Rs. 922.54 today. ", "Pay by 15 Nov! "]
    assert rest == "And the"
    done, rest = rc.split_sentences("आपकी राशि ₹500 है। धन्यवाद")
    assert done == ["आपकी राशि ₹500 है। "] and rest == "धन्यवाद"


def test_amounts_are_found_in_every_usual_form():
    found = rc.amounts_in("₹922.54, Rs. 1,500, INR 300, 5,000 rupees, ₹1.5 lakh and 2 crore rupees")
    assert found == [D("922.54"), D("1500.00"), D("300.00"), D("5000.00"), D("150000.00"), D("20000000.00")]
    assert rc.amounts_in("Pay by 15 November, call 1800 266 4332") == []


def test_amounts_may_be_rounded_to_the_rupee_but_not_changed():
    known = {D("922.54")}
    assert rc._matches(D("922.54"), known)
    assert rc._matches(D("923"), known) and rc._matches(D("922"), known)
    assert not rc._matches(D("925"), known)
    assert not rc._matches(D("922.50"), known)


def test_amounts_check_allows_tools_the_flow_and_the_caller():
    c = ctx()
    c.state.tool_numbers |= rc.numbers_in({"minimum_due": 922.54, "outstanding": "18,450.75"})
    c.state.values["amount"] = "4999"
    assert rc.CHECKS["amounts_from_tools"]("Your minimum due is ₹922.54 of ₹18,450.75.", c) == rc.PASS
    assert rc.CHECKS["amounts_from_tools"]("You'd like to dispute ₹4,999.", c) == rc.PASS
    verdict = rc.CHECKS["amounts_from_tools"]("Your minimum due is ₹925.", c)
    assert verdict.action == "stop" and verdict.retry


def test_long_numbers_are_masked_in_the_flows_language():
    c = ctx(("mask_long_numbers",))
    v = rc.CHECKS["mask_long_numbers"]("Your card 4321 5678 9012 3456 is blocked.", c)
    assert v == rc.Verdict.fix("Your card ending in 3456 is blocked.")
    assert rc.CHECKS["mask_long_numbers"]("Call 98765 43210 or 1800 266 4332.", c) == rc.PASS
    hindi = ctx(("mask_long_numbers",), language="hi")
    assert "आखिरी चार अंक 3456" in rc.CHECKS["mask_long_numbers"]("कार्ड 4321567890123456 है।", hindi).text


def test_promises_are_stopped_unless_negated():
    c = ctx(("no_promises",))
    for said in ["Your late fee has been waived.", "I guarantee it.", "You'll get a refund by Friday.", "Your request is approved.",
                 "Don't worry, your bounce charge will be waived.", "No problem; we'll waive it."]:
        assert rc.CHECKS["no_promises"](said, c).action == "stop", said
    for said in ["I can't guarantee that.", "The fee won't be waived, sorry.", "I'll raise a request for a refund.",
                 "Sorry, I cannot waive the charge."]:
        assert rc.CHECKS["no_promises"](said, c) == rc.PASS, said


def test_safe_line_follows_the_flow():
    assert rc.safe_line("no_promises", ctx()).startswith("I can't confirm that")
    assert rc.safe_line("no_promises", ctx(language="hi")).startswith("इस कॉल पर")
    assert rc.safe_line("no_promises", ctx(line="Ek minute, main check karti hoon.")) == "Ek minute, main check karti hoon."


async def _stream(*chunks):
    for c in chunks:
        yield c


def _text(chunk_text: str) -> llm.ChatChunk:
    return llm.ChatChunk(id="x", delta=llm.ChoiceDelta(role="assistant", content=chunk_text))


async def test_the_wrapper_checks_text_and_passes_tool_calls_through():
    c = ctx(("mask_long_numbers", "no_promises"))
    tool_call = llm.ChatChunk(
        id="x", delta=llm.ChoiceDelta(role="assistant", tool_calls=[llm.FunctionToolCall(name="go_to_goodbye", arguments="{}", call_id="c1")])
    )
    fired: list = []
    out = [
        x
        async for x in rc.checked(
            _stream(_text("Card 4321 5678 90"), _text("12 3456 noted. Also, your fee "), _text("is waived. Bye now."), tool_call),
            ["mask_long_numbers", "no_promises"],
            c,
            fired.append,
        )
    ]
    said = "".join(x for x in out if isinstance(x, str))
    assert said == "Card ending in 3456 noted. " + rc.SAFE_LINES["no_promises"][0] + " "
    assert out[-1] is tool_call and c.called_tools
    assert [(f.check, f.action) for f in fired] == [("mask_long_numbers", "fix"), ("no_promises", "stop")]


async def test_no_checks_means_the_stream_is_untouched():
    c = ctx(())
    chunks = [_text("Hello "), _text("there.")]
    assert [x async for x in rc.checked(_stream(*chunks), [], c)] == ["Hello there."]


def test_call_record_lists_checks_only_when_one_fired():
    rec = CallRecorder(call_id="AJ_1", channel="phone", flow_id="f", flow_version=1, flow_source="config-service", end_node_ids=set())
    assert "checks" not in rec.record()
    rec.reply_check("amounts_from_tools", "stop", "account_help")
    assert rec.record()["checks"] == [{"check": "amounts_from_tools", "node": "account_help", "action": "stop"}]


# ── a whole call ──────────────────────────────────────────────────────────────

VERIFY = [call("go_to_verify_details"), call("update_dob", year=1990, month=5, day=14), call("record_inputs", inputs=["4", "3", "2", "1"])]


async def test_a_call_where_the_llm_gets_things_wrong():
    h = credit_card_harness()
    h.state.flow = flow_with(["amounts_from_tools", "no_promises"], steps={"account_help": ["mask_long_numbers"]})
    fired: list = []
    h.state.on_check = lambda check, action, node: fired.append((check, action, node))
    h.llm.replies = {
        "what's my minimum due?": [
            "Your minimum due is ₹925. It's due on 15 November.",  # wrong: stopped
            "Your minimum due is ₹922.54, due on 15 November.",  # the one retry
        ],
        "roughly?": ["About ₹923."],  # rounded to the rupee: fine
        "and my card number?": ["Your card number is 4321 5678 9012 3456. Anything else?"],
        "can you waive the late fee?": ["Good news, your late fee has been waived. Anything else?"],
    }
    history = await converse(
        h,
        [*VERIFY, call("get_card_balance"), "what's my minimum due?", "roughly?", "and my card number?", "can you waive the late fee?"],
    )
    said = [i.text_content for i in history if i.type == "message" and i.role == "assistant"]
    heard = "\n".join(said)
    assert "₹925" not in heard
    assert "Let me just double-check that figure for you." in heard
    assert "Your minimum due is ₹922.54, due on 15 November." in heard
    assert "About ₹923." in heard
    assert "ending in 3456" in heard and "5678" not in heard
    assert "waived" not in heard and "I can't confirm that on this call" in heard
    assert fired == [
        ("amounts_from_tools", "stop", "account_help"),
        ("mask_long_numbers", "fix", "account_help"),
        ("no_promises", "stop", "account_help"),
    ]
    # The step is told how to say amounts so they can be checked.
    assert rc.AMOUNT_RULE in h.last_instructions("account_help")


async def test_a_wrong_amount_is_retried_only_once():
    h = credit_card_harness()
    h.state.flow = flow_with(["amounts_from_tools"])
    h.llm.replies = {"what's my minimum due?": ["It's ₹925.", "It's ₹930.", "It's ₹935."]}
    history = await converse(h, [*VERIFY, call("get_card_balance"), "what's my minimum due?"])
    heard = [i.text_content.strip() for i in history if i.type == "message" and i.role == "assistant"]
    assert heard[-2:] == ["Let me just double-check that figure for you.", "Let me just double-check that figure for you."]
    assert h.llm.replies["what's my minimum due?"] == ["It's ₹935."]  # no third try
