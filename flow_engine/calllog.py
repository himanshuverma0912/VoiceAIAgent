"""Call history: a content-free record of each call, sent to the config service.

What a record holds, and nothing else:
  - which flow and version ran, from where, on which runtime and channel
  - when the call started and how long it lasted
  - the steps it went through, with the time spent in each
  - how it ended (completed, caller hung up, ended early, time limit, error)
  - how many times each side spoke, and how fast the agent answered
  - usage totals (LLM tokens, seconds of speech-to-text, characters spoken)

Never: the caller's number, the room name (for phone calls it contains the
caller's number), anything said, or any value the caller gave. The config
service also refuses any field not on this list.
"""

from __future__ import annotations

import logging
import re
import time
from datetime import datetime, timezone
from typing import Any

import aiohttp

logger = logging.getLogger("call-log")

# How a call ended.
COMPLETED = "completed"  # the agent hung up from an end step
ENDED_EARLY = "ended_early"  # the agent hung up from another step (caller was done early)
CALLER_HUNG_UP = "caller_hung_up"
TIME_LIMIT = "time_limit"  # Flow Studio test calls stop at 5 minutes
ERROR = "error"
OTHER = "other"

_JOB_ID_RE = re.compile(r"^[A-Za-z0-9_-]{1,64}$")


def _percentile(values: list[float], p: float) -> float:
    s = sorted(values)
    k = (len(s) - 1) * p
    lo, hi = int(k), min(int(k) + 1, len(s) - 1)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


class CallRecorder:
    def __init__(
        self,
        *,
        call_id: str,
        channel: str,  # "phone" or "studio_test"
        flow_id: str,
        flow_version: int | None,
        flow_source: str,
        end_node_ids: set[str],
        runtime: str = "livekit",
        clock=time.time,
    ) -> None:
        self.call_id = call_id if _JOB_ID_RE.match(call_id or "") else ""
        self.channel, self.runtime = channel, runtime
        self.flow_id, self.flow_version, self.flow_source = flow_id, flow_version, flow_source
        self._end_nodes = end_node_ids
        self._clock = clock
        self.started = clock()
        self._steps: list[tuple[str, float]] = []
        self._caller_turns = 0
        self._agent_turns = 0
        self._latencies: list[float] = []
        self._agent_ended = False
        self._time_limit = False
        self._close_reason: str | None = None
        self._error = False

    # ── things that happen during the call ──

    def node_entered(self, node_id: str) -> None:
        self._steps.append((node_id, self._clock()))

    def agent_ended_call(self) -> None:
        self._agent_ended = True

    def time_limit_reached(self) -> None:
        self._time_limit = True

    def attach(self, session: Any) -> None:
        """Listen to an AgentSession's events (counts and timings only)."""

        def on_item(ev: Any) -> None:
            item = getattr(ev, "item", None)
            role = getattr(item, "role", None)
            if role == "user":
                self._caller_turns += 1
            elif role == "assistant":
                self._agent_turns += 1
                latency = (getattr(item, "metrics", None) or {}).get("e2e_latency")
                if isinstance(latency, (int, float)) and 0 <= latency < 600:
                    self._latencies.append(float(latency))

        def on_close(ev: Any) -> None:
            reason = getattr(ev, "reason", None)
            self._close_reason = getattr(reason, "value", reason)
            self._error = getattr(ev, "error", None) is not None

        session.on("conversation_item_added", on_item)
        session.on("close", on_close)

    # ── the record ──

    def outcome(self) -> str:
        if self._time_limit:
            return TIME_LIMIT
        if self._error or self._close_reason == "error":
            return ERROR
        if self._agent_ended:
            last = self._steps[-1][0] if self._steps else None
            return COMPLETED if last in self._end_nodes else ENDED_EARLY
        if self._close_reason == "participant_disconnected":
            return CALLER_HUNG_UP
        return OTHER

    def record(self, usage: Any = None) -> dict:
        ended = self._clock()
        steps = []
        for i, (node, entered) in enumerate(self._steps):
            left = self._steps[i + 1][1] if i + 1 < len(self._steps) else ended
            steps.append({"node": node, "ms": max(0, round((left - entered) * 1000))})
        latency = None
        if self._latencies:
            latency = {
                "p50": round(_percentile(self._latencies, 0.5) * 1000),
                "p95": round(_percentile(self._latencies, 0.95) * 1000),
                "count": len(self._latencies),
            }
        return {
            "call_id": self.call_id,
            "flow_id": self.flow_id,
            "flow_version": self.flow_version,
            "flow_source": self.flow_source,
            "channel": self.channel,
            "runtime": self.runtime,
            "started_at": datetime.fromtimestamp(self.started, timezone.utc).isoformat(timespec="milliseconds"),
            "duration_ms": max(0, round((ended - self.started) * 1000)),
            "outcome": self.outcome(),
            "steps": steps[:200],
            "turns": {"caller": self._caller_turns, "agent": self._agent_turns},
            "latency_ms": latency,
            "usage": _usage_totals(usage),
        }


def _usage_totals(usage: Any) -> dict:
    """Totals across models from AgentSession.usage (counts only)."""
    out = {"llm_input_tokens": 0, "llm_output_tokens": 0, "stt_audio_seconds": 0.0, "tts_characters": 0, "tts_audio_seconds": 0.0}
    for u in getattr(usage, "model_usage", None) or []:
        kind = getattr(u, "type", "")
        if kind == "llm_usage":
            out["llm_input_tokens"] += int(getattr(u, "input_tokens", 0) or 0)
            out["llm_output_tokens"] += int(getattr(u, "output_tokens", 0) or 0)
        elif kind == "stt_usage":
            out["stt_audio_seconds"] += float(getattr(u, "audio_duration", 0) or 0)
        elif kind == "tts_usage":
            out["tts_characters"] += int(getattr(u, "characters_count", 0) or 0)
            out["tts_audio_seconds"] += float(getattr(u, "audio_duration", 0) or 0)
    out["stt_audio_seconds"] = round(out["stt_audio_seconds"], 1)
    out["tts_audio_seconds"] = round(out["tts_audio_seconds"], 1)
    return out


async def send_record(url: str | None, token: str | None, record: dict, timeout: float = 3.0) -> bool:
    """POST the record to the config service. Failures are logged and dropped:
    a call's history is never kept anywhere else."""
    if not url or not token or not record.get("call_id"):
        return False
    try:
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=timeout)) as http:
            async with http.post(
                f"{url.rstrip('/')}/internal/v1/calls", json=record, headers={"Authorization": f"Bearer {token}"}
            ) as resp:
                if resp.status in (200, 201):
                    return True
                logger.warning("config service didn't take the call record (%s): %s", resp.status, (await resp.text())[:200])
    except Exception as e:  # never let history break the end of a call
        logger.warning("couldn't send the call record (%s)", type(e).__name__)
    return False
