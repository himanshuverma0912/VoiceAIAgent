"""Drop-resume (flow_engine/resume.py and ResumeAgent in runtime.py)."""

import asyncio
import base64
import json
import os
from pathlib import Path

import aiohttp
from aiohttp import web
from livekit.agents import AgentSession

from flow_engine import resume as rs
from flow_engine.calllog import CallRecorder
from flow_engine.resume import ResumePoint, ResumeStore, caller_key, choose_point, load_secret, seal, unseal, usable
from flow_engine.runtime import resume_agent
from flow_engine.spec import FlowError, parse_flow
from scripted_llm import call
from test_flow_engine import fake_dtmf_room, settle  # noqa: F401  (the keypad task needs a room)
from test_tools import credit_card_harness

ROOT = Path(__file__).resolve().parent.parent
SECRET = os.urandom(32)
ASHA = "+919876543210"


def card_flow(keep=("merchant",), enabled=True):
    data = json.loads((ROOT / "flows" / "credit-card-helpline.json").read_text())
    data["settings"]["resume"] = {"enabled": enabled, "keep": list(keep)}
    return parse_flow(data)


# ── keys and sealing ──────────────────────────────────────────────────────────


def test_secret_is_32_bytes_as_base64_or_hex():
    raw = os.urandom(32)
    assert load_secret(base64.b64encode(raw).decode()) == raw
    assert load_secret(raw.hex()) == raw
    assert load_secret("too-short") is None and load_secret(None) is None


def test_caller_key_hides_the_number_and_ignores_its_format():
    key = caller_key(SECRET, ASHA)
    assert len(key) == 64 and "9876543210" not in key
    assert caller_key(SECRET, "09876543210") == key == caller_key(SECRET, "98765 43210")
    assert caller_key(os.urandom(32), ASHA) != key
    assert caller_key(SECRET, "") == "" and caller_key(SECRET, "anonymous") == ""


def test_sealed_points_open_only_with_the_same_key_and_flow():
    point = ResumePoint(start="verify_details", then="dispute_failed", values={"merchant": "XYZ GLOBAL"})
    sealed = seal(SECRET, "credit_card_helpline", point)
    assert "XYZ" not in sealed and "dispute" not in base64.b64decode(sealed).decode("latin-1")
    assert unseal(SECRET, "credit_card_helpline", sealed) == point
    assert unseal(SECRET, "emi_payment_issue", sealed) is None  # filed under another flow
    assert unseal(os.urandom(32), "credit_card_helpline", sealed) is None  # key changed
    tampered = base64.b64encode(base64.b64decode(sealed)[:-1] + b"x").decode()
    assert unseal(SECRET, "credit_card_helpline", tampered) is None


# ── where a dropped call picks up ─────────────────────────────────────────────

BEFORE = ["triage", "verify_details", "verify"]


def test_before_verification_it_resumes_at_the_step_reached():
    flow = card_flow()
    assert choose_point(flow, ["triage"], {}, verified=False) == ResumePoint(start="triage")
    assert choose_point(flow, ["triage", "verify_details"], {}, verified=False).start == "verify_details"


def test_after_verification_the_caller_verifies_again_first():
    flow = card_flow()
    p = choose_point(flow, [*BEFORE, "account_help"], {}, verified=True)
    assert (p.start, p.then) == ("verify_details", "account_help")
    # Dropped inside an API step (raising the dispute): back to the step before it, never into it.
    p = choose_point(flow, [*BEFORE, "account_help", "dispute"], {}, verified=True)
    assert (p.start, p.then) == ("verify_details", "account_help")
    p = choose_point(flow, [*BEFORE, "account_help", "dispute", "dispute_failed"], {}, verified=True)
    assert (p.start, p.then) == ("verify_details", "dispute_failed")


def test_only_listed_values_are_kept_and_nothing_after_the_end():
    flow = card_flow(keep=("merchant", "amount"))
    values = {"merchant": "XYZ", "amount": "", "card_last4": "4321", "date_of_birth": "1990-05-14", "first_name": "Asha"}
    p = choose_point(flow, [*BEFORE, "account_help"], values, verified=True)
    assert p.values == {"merchant": "XYZ"}
    assert choose_point(flow, [*BEFORE, "account_help", "goodbye"], values, verified=True) is None


def test_a_resumed_call_that_drops_again_keeps_its_point():
    flow = card_flow()
    claimed = ResumePoint(start="verify_details", then="account_help", values={"merchant": "XYZ"})
    # Dropped while being asked "carry on or start over?": the same point again.
    assert choose_point(flow, [], {}, verified=False, claimed=claimed) == claimed
    # Dropped while verifying again: still goes back to account_help afterwards.
    p = choose_point(flow, ["verify_details"], {"merchant": "XYZ"}, verified=False, pending_then="account_help")
    assert (p.start, p.then, p.values) == ("verify_details", "account_help", {"merchant": "XYZ"})


def test_a_point_that_no_longer_fits_the_flow_is_dropped():
    flow = card_flow()
    assert usable(flow, ResumePoint(start="verify_details", then="account_help"))
    assert not usable(flow, ResumePoint(start="removed_step"))
    assert not usable(flow, ResumePoint(start="verify"))  # an API step


def test_keep_must_name_values_the_flow_defines():
    try:
        card_flow(keep=("merchant", "merchant", "pin_code"))
    except FlowError as e:
        assert e.problems == [
            "settings.resume.keep[1]: 'merchant' is already in the list",
            "settings.resume.keep[2]: 'pin_code' isn't a value this flow captures, collects or saves",
        ]
    else:
        raise AssertionError("expected problems")


def test_call_record_says_resumed_only_when_true():
    rec = CallRecorder(call_id="AJ_1", channel="phone", flow_id="f", flow_version=1, flow_source="config-service", end_node_ids=set())
    assert "resumed" not in rec.record() and "resume_saved" not in rec.record()
    rec.resumed()
    rec.resume_saved()
    r = rec.record()
    assert r["resumed"] is True and r["resume_saved"] is True


# ── a resumed call ────────────────────────────────────────────────────────────


async def resumed_call(h, point: ResumePoint, messages: list[str]) -> list:
    session = AgentSession(llm=h.llm)
    try:
        await session.start(resume_agent(h.state, point))
        await settle(session)
        for msg in messages:
            if session._is_closing():
                break
            await asyncio.wait_for(session.run(user_input=msg), 15)
            await asyncio.wait_for(settle(session), 20)
        return list(session.history.items)
    finally:
        await session.aclose()


VERIFY = [call("update_dob", year=1990, month=5, day=14), call("record_inputs", inputs=["4", "3", "2", "1"])]


async def test_carrying_on_verifies_again_then_returns_to_the_step():
    h = credit_card_harness()
    h.state.flow = card_flow()
    point = ResumePoint(start="verify_details", then="dispute_failed", values={"merchant": "XYZ GLOBAL"})
    await resumed_call(h, point, [call("continue_previous"), *VERIFY])
    # Not the verifying step's usual "next" (account_help): back where the call dropped.
    assert h.state.path == ["verify_details", "verify", "dispute_failed"], h.state.path
    assert h.state.values["merchant"] == "XYZ GLOBAL" and h.state.tools.verified
    asked = h.last_instructions("resume")
    assert "line dropped" in asked and "carry on" in asked and "merchant: XYZ GLOBAL" in asked
    assert "dispute_failed" in h.steps()


async def test_failing_verification_again_goes_to_on_error():
    h = credit_card_harness()
    h.state.flow = card_flow()
    point = ResumePoint(start="verify_details", then="dispute_failed")
    await resumed_call(h, point, [call("continue_previous"), call("update_dob", year=1990, month=5, day=15), VERIFY[1]])
    assert h.state.path[-2:] == ["verify", "not_verified"]
    assert not h.state.tools.verified


async def test_starting_over_forgets_the_point():
    h = credit_card_harness()
    h.state.flow = card_flow()
    point = ResumePoint(start="verify_details", then="account_help", values={"merchant": "XYZ GLOBAL"})
    await resumed_call(h, point, [call("start_over")])
    assert h.state.path == ["triage"] and h.state.values == {}
    assert h.state.resume_point is None and h.state.resume_then is None


# ── saving and claiming through the config service ────────────────────────────


async def test_save_and_claim_through_the_config_service():
    stored: dict[str, dict] = {}
    seen_bodies: list[str] = []

    async def put(request: web.Request) -> web.Response:
        assert request.headers["Authorization"] == "Bearer rt"
        body = await request.text()
        seen_bodies.append(body)
        stored[request.match_info["key"]] = json.loads(body)
        return web.json_response({}, status=201)

    async def claim(request: web.Request) -> web.Response:
        row = stored.pop(request.match_info["key"], None)
        if row is None or row["flow_id"] != (await request.json())["flow_id"]:
            return web.json_response({"error": "none"}, status=404)
        return web.json_response({"sealed": row["sealed"], "flow_version": row["flow_version"]})

    app = web.Application()
    app.router.add_put("/internal/v1/resume/{key}", put)
    app.router.add_post("/internal/v1/resume/{key}/claim", claim)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    try:
        store = ResumeStore(f"http://127.0.0.1:{port}", "rt", SECRET)
        flow = card_flow()
        point = ResumePoint(start="verify_details", then="account_help", values={"merchant": "XYZ GLOBAL"})
        async with aiohttp.ClientSession() as http:
            assert await store.save(http, flow.flow_id, 3, ASHA, point)
            # The config service sees neither the number nor the values.
            assert "9876543210" not in seen_bodies[0] and "XYZ" not in seen_bodies[0]
            assert await store.claim(http, flow, "09876543210") == point
            assert await store.claim(http, flow, ASHA) is None  # used once
            # Withheld numbers never resume; without a key nothing is sent.
            assert not await store.save(http, flow.flow_id, 3, "anonymous", point)
            assert not ResumeStore(f"http://127.0.0.1:{port}", "rt", None).enabled
        # Unreachable config service: the call just starts fresh.
        async with aiohttp.ClientSession() as http:
            gone = ResumeStore("http://127.0.0.1:9", "rt", SECRET, timeout=0.5)
            assert await gone.claim(http, flow, ASHA) is None
            assert not await gone.save(http, flow.flow_id, 3, ASHA, point)
    finally:
        await runner.cleanup()


def test_resume_minutes_is_fifteen():
    assert rs.RESUME_MINUTES == 15
