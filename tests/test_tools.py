"""Tools and API steps (flow_engine/tools.py and the api node in runtime.py).

The demo core-banking server (infra/demo-bank/server.py) answers both kinds of
integration: its REST side is called over HTTP here, its MCP side through the
real MCP client, and the whole credit card flow runs with a scripted LLM.
"""

import asyncio
import json
import socket
import sys
from pathlib import Path

import aiohttp
import pytest
import uvicorn

from flow_engine.runtime import CallState, tool_for_llm
from flow_engine.spec import ToolUse, load_flow
from flow_engine.tools import ToolCaller, ToolFailed, ToolUseRecord, parse_integrations
from scripted_llm import call
from test_flow_engine import Harness, converse

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "infra" / "demo-bank"))
import server as demo_bank  # noqa: E402

TOKEN = "test-token"
ENV = {"INTEGRATION_DEMO_BANK_TOKEN": f"Bearer {TOKEN}", "LIVEKIT_API_SECRET": "never-send-me"}


def integration_file(name: str, url: str) -> dict:
    data = json.loads((ROOT / "infra" / "integrations" / f"{name}.json").read_text())
    data["url"] = url
    return data


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture
async def bank(monkeypatch):
    """The demo bank on a local port, requiring the test token."""
    monkeypatch.setattr(demo_bank, "TOKEN", TOKEN)
    port = free_port()
    server = uvicorn.Server(uvicorn.Config(demo_bank.build_app(), host="127.0.0.1", port=port, log_level="warning"))
    task = asyncio.create_task(server.serve())
    for _ in range(100):
        if server.started:
            break
        await asyncio.sleep(0.05)
    yield f"http://127.0.0.1:{port}"
    server.should_exit = True
    await task


ASHA = {"card_last4": "4321", "date_of_birth": "1990-05-14"}


async def test_rest_integration(bank):
    used: list[ToolUseRecord] = []
    integrations = parse_integrations({"demo_bank": integration_file("demo_bank_rest", bank)})
    async with aiohttp.ClientSession() as http:
        tools = ToolCaller(integrations, http=http, env=ENV, on_used=used.append)
        # General help: no verification needed (GET, inputs in the query).
        faq = await tools.call("demo_bank.search_faq", {"question": "what is the late fee"})
        assert "late fee" in faq["answer"]
        # Sensitive tools wait for verification, even if asked directly.
        with pytest.raises(ToolFailed, match="hasn't been verified"):
            await tools.call("demo_bank.get_card_balance", ASHA)
        with pytest.raises(ToolFailed, match="no customer matches"):
            await tools.call("demo_bank.verify_customer", {**ASHA, "date_of_birth": "1990-05-15"})
        assert (await tools.call("demo_bank.verify_customer", ASHA))["first_name"] == "Asha"
        tools.verified = True
        balance = await tools.call("demo_bank.get_card_balance", ASHA)
        assert balance["minimum_due"] == 922.54 and balance["overdue"] is False
        # Text from the flow becomes the type the tool expects.
        txns = await tools.call("demo_bank.recent_transactions", {**ASHA, "count": "2"})
        assert len(txns["transactions"]) == 2
        # At most max_calls per call.
        await tools.call("demo_bank.recent_transactions", ASHA)
        await tools.call("demo_bank.recent_transactions", ASHA)
        with pytest.raises(ToolFailed, match="as many times as allowed"):
            await tools.call("demo_bank.recent_transactions", ASHA)
        with pytest.raises(ToolFailed, match="missing merchant"):
            await tools.call("demo_bank.raise_dispute", ASHA)
    # Only which tool, whether it worked and how long: no inputs, no answers.
    assert [(u.tool, u.ok) for u in used][:3] == [
        ("demo_bank.search_faq", True),
        ("demo_bank.verify_customer", False),
        ("demo_bank.verify_customer", True),
    ]
    assert "4321" not in repr(used) and "1990" not in repr(used)


async def test_credentials_only_from_integration_variables(bank):
    data = integration_file("demo_bank_rest", bank)
    data["auth_env"] = "LIVEKIT_API_SECRET"  # the config service refuses this; the agent does too
    tools = ToolCaller(parse_integrations({"demo_bank": data}), env=ENV)
    with pytest.raises(ToolFailed, match="unauthorized"):
        await tools.call("demo_bank.search_faq", {"question": "emi"})
    await tools.aclose()


async def test_mcp_integration(bank):
    integrations = parse_integrations({"demo_bank": integration_file("demo_bank", bank + "/mcp")})
    tools = ToolCaller(integrations, env=ENV)
    tools.warm_up()
    try:
        assert (await tools.call("demo_bank.verify_customer", ASHA)) == {"verified": True, "first_name": "Asha"}
        with pytest.raises(ToolFailed):
            await tools.call("demo_bank.verify_customer", {"card_last4": "0000", "date_of_birth": "2000-01-01"})
        tools.verified = True
        assert (await tools.call("demo_bank.get_card_balance", {"card_last4": "8765", "date_of_birth": "1985-11-02"}))["overdue"] is True
    finally:
        await tools.aclose()


async def test_mcp_server_down():
    integrations = parse_integrations({"demo_bank": integration_file("demo_bank", f"http://127.0.0.1:{free_port()}/mcp")})
    tools = ToolCaller(integrations, env=ENV)
    with pytest.raises(ToolFailed, match="isn't reachable"):
        await tools.call("demo_bank.search_faq", {"question": "emi"})
    await tools.aclose()


def test_llm_never_sees_locked_inputs():
    flow = load_flow(ROOT / "flows" / "credit-card-helpline.json")
    integrations = parse_integrations({"demo_bank": integration_file("demo_bank", "http://x/mcp")})
    state = CallState(flow=flow, tools=ToolCaller(integrations, env={}))
    balance = tool_for_llm(state, flow.nodes["account_help"].tools[1])
    schema = balance.info.raw_schema
    assert schema["name"] == "recent_transactions"
    assert set(schema["parameters"]["properties"]) == {"count"}  # card and date of birth are the flow's
    # A tool the call doesn't have (integration switched off) isn't offered.
    assert tool_for_llm(state, ToolUse(tool="crm.lookup")) is None


class LocalBank(ToolCaller):
    """The demo bank's functions, called in-process (no network) for flow tests."""

    async def _call_mcp(self, integration, tool, args):
        try:
            return getattr(demo_bank, tool.name)(**args)
        except demo_bank.NoMatch as e:
            raise ToolFailed(str(e)) from None


def credit_card_harness() -> Harness:
    h = Harness("credit-card-helpline.json")
    h.used = []
    h.state.tools = LocalBank(
        parse_integrations({"demo_bank": integration_file("demo_bank", "http://demo-bank:8000/mcp")}), env={}, on_used=h.used.append
    )
    return h


async def test_credit_card_flow_with_tools():
    h = credit_card_harness()
    history = await converse(
        h,
        [
            call("search_faq", question="what is the minimum due"),  # every step has it
            call("go_to_verify_details"),
            call("update_dob", year=1990, month=5, day=14),
            call("record_inputs", inputs=["4", "3", "2", "1"]),
            # verify runs by itself, then the caller asks for the balance.
            call("get_card_balance", card_last4="9999"),  # the LLM can't choose the card: ignored
            call(
                "go_to_dispute",
                merchant="XYZ GLOBAL SERVICES",
                amount="4999",
                payment_date="two days ago",
            ),
            call("go_to_goodbye"),
        ],
    )
    path = h.state.path
    assert path == ["triage", "verify_details", "verify", "account_help", "dispute", "dispute_raised", "goodbye"], path
    assert h.state.values["first_name"] == "Asha"
    assert h.state.values["reference"].startswith("DSP-")
    assert h.state.tools.verified
    tools_used = [(u.tool, u.ok) for u in h.used]
    assert tools_used == [
        ("demo_bank.search_faq", True),
        ("demo_bank.verify_customer", True),
        ("demo_bank.get_card_balance", True),
        ("demo_bank.raise_dispute", True),
    ]
    # The balance the LLM got is Asha's (the flow's card digits, not "9999").
    outputs = [i for i in history if i.type == "function_call_output" and i.name == "get_card_balance"]
    assert '"minimum_due": 922.54' in outputs[0].output
    # The API step's line was said, and the saved reference reached the next step.
    said = [i.text_content for i in history if i.type == "message" and i.role == "assistant"]
    assert "One moment while I check those details." in said
    assert h.state.values["reference"] in h.last_instructions("dispute_raised")


async def test_failed_verification_goes_to_on_error():
    h = credit_card_harness()
    await converse(
        h,
        [
            call("go_to_verify_details"),
            call("update_dob", year=1990, month=5, day=15),  # wrong date
            call("record_inputs", inputs=["4", "3", "2", "1"]),
            "what's my balance?",
        ],
    )
    assert h.state.path[-2:] == ["verify", "not_verified"]
    assert not h.state.tools.verified
    # On the next step there are no balance tools at all.
    assert "get_card_balance" not in h.llm.requests[-1]["tools"]
    assert [(u.tool, u.ok) for u in h.used] == [("demo_bank.verify_customer", False)]


async def test_api_step_without_integrations_goes_to_on_error():
    h = Harness("credit-card-helpline.json")  # no tools on this call at all
    await converse(
        h,
        [call("go_to_verify_details"), call("update_dob", year=1990, month=5, day=14), call("record_inputs", inputs=["4", "3", "2", "1"])],
    )
    assert h.state.path[-2:] == ["verify", "not_verified"]
    assert "search_faq" not in h.llm.requests[0]["tools"]
