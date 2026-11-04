"""The demo EMI MCP servers (examples/emi-mcp): both roles, through the
agent's own ToolCaller, with the tool rules makers paste into Flow Studio; and
the EMI flow that uses them."""

from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

import pytest
import uvicorn

from flow_engine import load_flow
from flow_engine.tools import ToolCaller, ToolFailed, parse_integrations
from test_tools import free_port

EXAMPLE = Path(__file__).resolve().parent.parent / "examples" / "emi-mcp"
sys.path.insert(0, str(EXAMPLE))
import emi_mcp  # noqa: E402

TOKEN = "test-token-0123456789"
RAVI = {"loan_last4": "8765", "date_of_birth": "1985-11-02"}
ASHA = {"loan_last4": "4321", "date_of_birth": "14/05/1990"}


@pytest.fixture
async def servers():
    """Both roles on local ports."""
    urls, running = {}, []
    for role in ("core", "helpdesk"):
        port = free_port()
        server = uvicorn.Server(uvicorn.Config(emi_mcp.build_app(role, TOKEN), host="127.0.0.1", port=port, log_level="warning"))
        running.append((server, asyncio.create_task(server.serve())))
        urls[role] = f"http://127.0.0.1:{port}/mcp"
    for server, _ in running:
        for _ in range(100):
            if server.started:
                break
            await asyncio.sleep(0.05)
    yield urls
    for server, task in running:
        server.should_exit = True
        await task


def integrations(urls: dict[str, str], token: str = TOKEN):
    return parse_integrations(
        {
            f"emi_{role}": {
                "kind": "mcp",
                "url": url,
                "auth_header": "Authorization",
                "auth_env": "INTEGRATION_EMI_TOKEN",
                "tools": json.loads((EXAMPLE / f"emi_{role}.tools.json").read_text()),
            }
            for role, url in urls.items()
        }
    ), {"INTEGRATION_EMI_TOKEN": f"Bearer {token}"}


async def test_core_and_helpdesk(servers):
    found, env = integrations(servers)
    tools = ToolCaller(found, env=env)
    tools.warm_up()
    try:
        assert (await tools.call("emi_core.verify_borrower", RAVI))["first_name"] == "Ravi"
        with pytest.raises(ToolFailed, match="no loan matches"):
            await tools.call("emi_core.verify_borrower", {**RAVI, "date_of_birth": "1985-11-03"})
        with pytest.raises(ToolFailed, match="hasn't been verified"):
            await tools.call("emi_core.get_emi_status", RAVI)
        tools.verified = True
        status = await tools.call("emi_core.get_emi_status", RAVI)
        assert status["last_emi_status"].startswith("bounced") and status["total_to_pay_now"] == 12990.0
        debits = await tools.call("emi_core.check_recent_debits", ASHA)
        assert debits["duplicate_found"] is True and debits["duplicate_amount"] == 24850.0
        dispute = await tools.call("emi_core.raise_debit_dispute", {**ASHA, "debit_date": debits["duplicate_date"]})
        assert dispute["reference"].startswith("EMID-")

        assert "590" in (await tools.call("emi_helpdesk.emi_faq", {"question": "my NACH bounced"}))["answer"]
        link = await tools.call("emi_helpdesk.send_payment_link", {"mobile": "+919876543210", "loan_last4": "8765", "amount": "12990"})
        assert link["sent_to"] == "******3210"
        assert (await tools.call("emi_helpdesk.request_callback", {"mobile": "+919876543210", "reason": "EMI date change"}))["call_within"]
    finally:
        await tools.aclose()


async def test_token_required(servers):
    found, env = integrations(servers, token="wrong-token-0123456789")
    tools = ToolCaller(found, env=env)
    with pytest.raises(ToolFailed, match="isn't reachable"):
        await tools.call("emi_helpdesk.emi_faq", {"question": "late fee"})
    await tools.aclose()


def test_tool_rules_match_the_flow():
    flow = load_flow(EXAMPLE / "emi-payment-issue.json")
    assert flow.flow_id == "emi_payment_issue"
    rules = {f"emi_{r}.{t['name']}": t for r in ("core", "helpdesk") for t in json.loads((EXAMPLE / f"emi_{r}.tools.json").read_text())}
    # Inputs that pick whose loan it is are always set by the flow, never the AI.
    for ref in ("emi_core.get_emi_status", "emi_core.check_recent_debits", "emi_core.raise_debit_dispute"):
        assert rules[ref]["sensitive"] and {"loan_last4", "date_of_birth"} <= set(rules[ref]["locked"])
    assert rules["emi_helpdesk.send_payment_link"]["locked"] == ["mobile", "loan_last4"]
