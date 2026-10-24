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

    def _fake_end_call(self, goodbye: str, extra: str):
        # Same tool name and role as the real EndCallTool, but no room to delete.
        @function_tool(name="end_call", description=f"End the call. {extra}")
        async def end_call() -> None:
            self.ended.append(goodbye)

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
            call("go_to_closing"),
            call("end_call"),
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
    assert h.ended == [
        "Thank the caller for contacting the Complaint Department, tell them their concern has been "
        "recorded, and say goodbye, in the language they are speaking."
    ]

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
            call("end_call"),
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
