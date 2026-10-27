"""Call history records: what they contain, how a call's outcome is decided,
and that nothing personal ends up in them."""

import asyncio
import json
from types import SimpleNamespace

from aiohttp import web

from flow_engine.calllog import (
    CALLER_HUNG_UP,
    COMPLETED,
    ENDED_EARLY,
    ERROR,
    OTHER,
    TIME_LIMIT,
    CallRecorder,
    send_record,
)


class Clock:
    def __init__(self, t: float = 1_760_000_000.0) -> None:
        self.t = t

    def __call__(self) -> float:
        return self.t


class FakeSession:
    def __init__(self) -> None:
        self.handlers: dict[str, list] = {}

    def on(self, event, fn):
        self.handlers.setdefault(event, []).append(fn)

    def emit(self, event, ev):
        for fn in self.handlers.get(event, []):
            fn(ev)


def item(role, text="", latency=None):
    return SimpleNamespace(item=SimpleNamespace(role=role, content=[text], metrics={"e2e_latency": latency} if latency is not None else {}))


def close(reason, error=None):
    return SimpleNamespace(reason=SimpleNamespace(value=reason), error=error)


def recorder(clock, **kw):
    args = dict(
        call_id="AJ_abc123",
        channel="phone",
        flow_id="complaint_helpline",
        flow_version=3,
        flow_source="config-service",
        end_node_ids={"goodbye"},
        clock=clock,
    )
    args.update(kw)
    return CallRecorder(**args)


def usage():
    return SimpleNamespace(
        model_usage=[
            SimpleNamespace(type="llm_usage", input_tokens=1200, output_tokens=150),
            SimpleNamespace(type="llm_usage", input_tokens=300, output_tokens=50),
            SimpleNamespace(type="stt_usage", audio_duration=41.26),
            SimpleNamespace(type="tts_usage", characters_count=640, audio_duration=38.04),
            SimpleNamespace(type="interruption_usage"),
        ]
    )


def test_a_completed_call():
    clock = Clock()
    rec = recorder(clock)
    session = FakeSession()
    rec.attach(session)

    rec.node_entered("greet")
    clock.t += 4.0
    session.emit("conversation_item_added", item("assistant", "Namaste", latency=None))
    session.emit("conversation_item_added", item("user", "my name is Asha, 9876543210"))
    session.emit("conversation_item_added", item("assistant", "Thank you", latency=0.8))
    rec.node_entered("collect_details")
    clock.t += 30.0
    session.emit("conversation_item_added", item("user", "..."))
    session.emit("conversation_item_added", item("assistant", "...", latency=1.2))
    rec.node_entered("goodbye")
    clock.t += 5.0
    rec.agent_ended_call()
    session.emit("close", close("job_shutdown"))

    r = rec.record(usage())
    assert r["outcome"] == COMPLETED
    assert r["duration_ms"] == 39000
    assert r["steps"] == [
        {"node": "greet", "ms": 4000},
        {"node": "collect_details", "ms": 30000},
        {"node": "goodbye", "ms": 5000},
    ]
    assert r["turns"] == {"caller": 2, "agent": 3}
    assert r["latency_ms"] == {"p50": 1000, "p95": 1180, "count": 2}
    assert r["usage"] == {
        "llm_input_tokens": 1500,
        "llm_output_tokens": 200,
        "stt_audio_seconds": 41.3,
        "tts_characters": 640,
        "tts_audio_seconds": 38.0,
    }
    assert r["started_at"].endswith("+00:00")
    assert r["flow_version"] == 3 and r["channel"] == "phone" and r["runtime"] == "livekit"


def test_outcomes():
    def outcome(steps, *, ended=False, limit=False, reason="job_shutdown", error=None):
        rec = recorder(Clock())
        s = FakeSession()
        rec.attach(s)
        for n in steps:
            rec.node_entered(n)
        if ended:
            rec.agent_ended_call()
        if limit:
            rec.time_limit_reached()
        s.emit("close", close(reason, error))
        return rec.outcome()

    assert outcome(["greet", "goodbye"], ended=True) == COMPLETED
    assert outcome(["greet"], ended=True) == ENDED_EARLY
    assert outcome(["greet"], reason="participant_disconnected") == CALLER_HUNG_UP
    assert outcome(["greet"], limit=True, reason="participant_disconnected") == TIME_LIMIT
    assert outcome(["greet"], reason="error", error=RuntimeError("tts")) == ERROR
    assert outcome(["greet"]) == OTHER
    # The agent said goodbye, then the caller hung up before the room closed.
    assert outcome(["greet", "goodbye"], ended=True, reason="participant_disconnected") == COMPLETED


def test_nothing_personal_is_recorded():
    """The record has a fixed shape. Words, numbers and collected values never
    appear, and a room name passed by mistake as the call id is dropped."""
    clock = Clock()
    rec = recorder(clock, call_id="_inbound_+919876543210_xyZ")  # a SIP room name
    s = FakeSession()
    rec.attach(s)
    rec.node_entered("greet")
    s.emit("conversation_item_added", item("user", "my number is 9876543210, email asha@example.com"))
    s.emit("conversation_item_added", item("assistant", "Got it, Asha", latency=0.9))
    r = rec.record(usage())

    text = json.dumps(r)
    for secret in ("9876543210", "asha", "Asha", "example.com", "Got it"):
        assert secret not in text
    assert r["call_id"] == ""  # not a job id → not sent at all
    assert set(r) == {
        "call_id", "flow_id", "flow_version", "flow_source", "channel", "runtime",
        "started_at", "duration_ms", "outcome", "steps", "tools", "turns", "latency_ms", "usage",
    }


def test_tools_are_recorded_by_name_only():
    from flow_engine.tools import ToolUseRecord

    rec = recorder(Clock())
    rec.tool_used(ToolUseRecord("demo_bank.get_card_balance", True, 231))
    rec.tool_used(ToolUseRecord("demo_bank.raise_dispute", False, 5000))
    assert rec.record()["tools"] == [
        {"tool": "demo_bank.get_card_balance", "ok": True, "ms": 231},
        {"tool": "demo_bank.raise_dispute", "ok": False, "ms": 5000},
    ]


def test_no_latency_without_answers():
    rec = recorder(Clock())
    assert rec.record()["latency_ms"] is None
    assert rec.record()["usage"]["llm_input_tokens"] == 0


def test_send_record_posts_to_the_config_service():
    async def run():
        got = {}

        async def handler(request):
            got["auth"] = request.headers.get("Authorization")
            got["body"] = await request.json()
            return web.json_response({"ok": True}, status=201)

        app = web.Application()
        app.router.add_post("/internal/v1/calls", handler)
        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        port = site._server.sockets[0].getsockname()[1]
        try:
            rec = recorder(Clock()).record()
            assert await send_record(f"http://127.0.0.1:{port}/", "rt-token", rec) is True
            assert got["auth"] == "Bearer rt-token"
            assert got["body"]["call_id"] == "AJ_abc123"
            # No URL, no token, or no call id: nothing is sent.
            assert await send_record(None, "rt-token", rec) is False
            assert await send_record(f"http://127.0.0.1:{port}", "rt-token", {**rec, "call_id": ""}) is False
            # A config service that's down doesn't raise.
            await runner.cleanup()
            assert await send_record(f"http://127.0.0.1:{port}", "rt-token", rec, timeout=1) is False
        finally:
            await runner.cleanup()

    asyncio.run(run())
