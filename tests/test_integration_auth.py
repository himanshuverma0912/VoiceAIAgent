"""Signing in to a workspace's own integrations: tokens sealed for the agent,
OAuth client credentials, and the addresses a restricted integration may not
reach. Uses the demo bank on a local port (see test_tools.py)."""

from __future__ import annotations

import asyncio
import base64
import json
import logging
from pathlib import Path

import aiohttp
import pytest
from aiohttp import web

from flow_engine import secrets
from flow_engine.tools import (
    ToolCaller,
    ToolFailed,
    _Refused,
    _SafeResolver,
    check_address,
    integrations_to_json,
    parse_integrations,
)
from test_tools import ASHA, bank, demo_bank, free_port, integration_file  # noqa: F401 - bank is a fixture

FIXTURE = json.loads((Path(__file__).parent / "secrets_fixture.json").read_text())
KEY_ENV = {"INTEGRATION_SECRETS_KEY": FIXTURE["private_key"]}


def sealed_for(integration_id: str, secret: str) -> str:
    public = secrets.private_key(FIXTURE["private_key"]).public_key()
    return secrets.seal(public, integration_id, secret)


def workspace_integration(url: str, auth: dict, *, kind: str = "mcp", restricted: bool = False) -> dict:
    data = integration_file("demo_bank" if kind == "mcp" else "demo_bank_rest", url)
    data.pop("auth_header", None)
    data.pop("auth_env", None)
    data["auth"] = auth
    if restricted:
        data["restricted"] = True
    return data


def test_opens_what_the_config_service_sealed():
    key = secrets.private_key(FIXTURE["private_key"])
    assert secrets.open_sealed(key, FIXTURE["integration_id"], FIXTURE["sealed"]) == FIXTURE["plaintext"]
    with pytest.raises(secrets.SecretUnavailable):
        secrets.open_sealed(key, "another_integration", FIXTURE["sealed"])
    with pytest.raises(secrets.SecretUnavailable):
        secrets.open_sealed(None, FIXTURE["integration_id"], FIXTURE["sealed"])
    other, _ = secrets.keygen()
    with pytest.raises(secrets.SecretUnavailable):
        secrets.open_sealed(secrets.private_key(other), FIXTURE["integration_id"], FIXTURE["sealed"])


async def test_sealed_token(bank, caplog):  # noqa: F811
    auth = {"kind": "header", "header": "Authorization", "sealed": sealed_for("demo_bank", "Bearer test-token")}
    for kind, url in (("mcp", bank + "/mcp"), ("http", bank)):
        tools = ToolCaller(parse_integrations({"demo_bank": workspace_integration(url, auth, kind=kind)}), env=KEY_ENV)
        try:
            assert (await tools.call("demo_bank.verify_customer", ASHA))["first_name"] == "Asha"
        finally:
            await tools.aclose()

    # Without the key, or sealed for another integration: no call, and the
    # reason in the log (never the token).
    caplog.set_level(logging.INFO)
    moved = {**auth, "sealed": sealed_for("core_banking", "Bearer test-token")}
    for env, data in ((KEY_ENV, workspace_integration(bank, moved, kind="http")), ({}, workspace_integration(bank, auth, kind="http"))):
        tools = ToolCaller(parse_integrations({"demo_bank": data}), env=env)
        with pytest.raises(ToolFailed, match="isn't reachable"):
            await tools.call("demo_bank.search_faq", {"question": "emi"})
        await tools.aclose()
    assert "can't be opened" in caplog.text and "test-token" not in caplog.text


@pytest.fixture
async def token_server():
    """An OAuth token endpoint for client "flow-agent" with secret "s3:cret"."""
    issued: list[str] = []

    async def token(request: web.Request) -> web.Response:
        form = await request.post()
        basic = request.headers.get("Authorization", "")
        want = "Basic " + base64.b64encode(b"flow-agent:s3%3Acret").decode()
        if basic != want or form.get("grant_type") != "client_credentials" or form.get("scope") != "loans.read":
            return web.json_response({"error": "invalid_client"}, status=401)
        issued.append("tok-1")
        return web.json_response({"access_token": "tok-1", "token_type": "Bearer", "expires_in": 300})

    app = web.Application()
    app.router.add_post("/token", token)
    runner = web.AppRunner(app)
    await runner.setup()
    port = free_port()
    await web.TCPSite(runner, "127.0.0.1", port).start()
    yield f"http://127.0.0.1:{port}/token", issued
    await runner.cleanup()


async def test_oauth_client_credentials(bank, token_server, monkeypatch):  # noqa: F811
    token_url, issued = token_server
    monkeypatch.setattr(demo_bank, "TOKEN", "tok-1")
    auth = {
        "kind": "oauth",
        "token_url": token_url,
        "client_id": "flow-agent",
        "scope": "loans.read",
        "sealed": sealed_for("demo_bank", "s3:cret"),
    }
    tools = ToolCaller(parse_integrations({"demo_bank": workspace_integration(bank + "/mcp", auth)}), env=KEY_ENV)
    tools.warm_up()
    try:
        assert (await tools.call("demo_bank.verify_customer", ASHA))["verified"] is True
        assert "late fee" in (await tools.call("demo_bank.search_faq", {"question": "late fee"}))["answer"]
    finally:
        await tools.aclose()
    assert issued == ["tok-1"]  # once per call

    wrong = {**auth, "sealed": sealed_for("demo_bank", "not-the-secret")}
    tools = ToolCaller(parse_integrations({"demo_bank": workspace_integration(bank, wrong, kind="http")}), env=KEY_ENV)
    with pytest.raises(ToolFailed, match="isn't reachable"):
        await tools.call("demo_bank.search_faq", {"question": "emi"})
    await tools.aclose()


async def test_restricted_integrations_stay_off_this_machine(bank, caplog):  # noqa: F811
    caplog.set_level(logging.INFO)
    auth = {"kind": "header", "sealed": sealed_for("demo_bank", "Bearer test-token")}
    local = bank.replace("127.0.0.1", "localhost")
    for kind, url in (("mcp", bank + "/mcp"), ("http", bank), ("mcp", local + "/mcp"), ("http", local)):
        data = workspace_integration(url, auth, kind=kind, restricted=True)
        tools = ToolCaller(parse_integrations({"demo_bank": data}), env=KEY_ENV)
        with pytest.raises(ToolFailed, match="isn't reachable"):
            await tools.call("demo_bank.search_faq", {"question": "emi"})
        await tools.aclose()
    assert "refused: the integration's address" in caplog.text and "test-token" not in caplog.text

    # A token URL on this machine is refused too.
    oauth = {"kind": "oauth", "token_url": bank + "/token", "client_id": "x", "sealed": sealed_for("demo_bank", "s")}
    tools = ToolCaller(parse_integrations({"demo_bank": workspace_integration("http://10.255.255.1/api", oauth, kind="http", restricted=True)}), env=KEY_ENV)
    with pytest.raises(ToolFailed, match="isn't reachable"):
        await tools.call("demo_bank.search_faq", {"question": "emi"})
    await tools.aclose()
    assert "refused: the token URL" in caplog.text


@pytest.mark.parametrize(
    "url",
    [
        "http://127.0.0.1/mcp",
        "http://[::1]/mcp",
        "http://[::ffff:127.0.0.1]/mcp",
        "http://169.254.169.254/latest/meta-data",
        "http://[fd00:ec2::254]/",
        "http://0.0.0.0:8080/",
        "http://localhost/mcp",
        "http://api.localhost/mcp",
        "http://config-service:8080/internal/v1/resolve",
        "http://metadata.google.internal/",
    ],
)
async def test_check_address_refuses(url):
    with pytest.raises(_Refused):
        await check_address(url)


async def test_check_address_allows_private_networks():
    await check_address("http://10.20.3.4:8080/mcp")  # an enterprise cluster's internal address
    await check_address("https://192.168.1.10/mcp")


async def test_safe_resolver_leaves_out_this_machine(monkeypatch):
    async def resolve(self, host, port=0, family=0):
        return [{"hostname": host, "host": "127.0.0.1", "port": port, "family": 2, "proto": 0, "flags": 0}]

    monkeypatch.setattr("aiohttp.resolver.DefaultResolver.resolve", resolve)
    resolver = _SafeResolver()
    with pytest.raises(OSError):
        await resolver.resolve("sneaky.example", 80)
    with pytest.raises(OSError):
        await resolver.resolve("localhost", 80)
    await resolver.close()


def test_cache_round_trip():
    auth = {"kind": "oauth", "token_url": "https://idp.bank/token", "client_id": "a", "scope": "s", "sealed": "v1.xyz"}
    data = {"loans": workspace_integration("https://loans.bank/mcp", auth, restricted=True)}
    parsed = parse_integrations(data)
    assert parsed["loans"].restricted and parsed["loans"].auth.token_url == "https://idp.bank/token"
    assert parse_integrations(integrations_to_json(parsed)) == parsed
    # An auth entry the agent can't use: the integration is left out.
    assert parse_integrations({"loans": {**data["loans"], "auth": {"kind": "magic", "sealed": "v1.x"}}}) == {}


async def test_sign_in_once_per_call(bank):  # noqa: F811
    auth = {"kind": "header", "sealed": sealed_for("demo_bank", "Bearer test-token")}
    tools = ToolCaller(parse_integrations({"demo_bank": workspace_integration(bank, auth, kind="http")}), env=KEY_ENV)
    async with aiohttp.ClientSession():
        results = await asyncio.gather(*(tools.call("demo_bank.search_faq", {"question": "emi"}) for _ in range(3)))
    assert all("answer" in r for r in results) and len(tools._sign_in) == 1
    await tools.aclose()
