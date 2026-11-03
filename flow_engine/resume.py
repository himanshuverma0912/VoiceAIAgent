"""Drop-resume: a caller who hangs up in the middle of a flow can carry on
where they left off if they call the same flow back within 15 minutes.

What is kept, for at most 15 minutes and used at most once:
  - the step to start at and, if the caller had been verified, the step to go
    back to once they verify again;
  - the values the flow lists in settings.resume.keep, nothing else.

How:
  - sealed with AES-GCM under RESUME_KEY (the agent's .env), so the config
    service's database only ever holds ciphertext;
  - filed under an HMAC of the caller's number, never the number itself;
  - claiming it at the start of the next call deletes it.

Never kept: what was said, or the fact that the caller was verified. Caller ID
can be spoofed, so a resumed caller who had been verified goes through
verification again before reaching the step they left. And a call never
resumes inside an API step: that could repeat an action (raising a dispute
twice), so it resumes at the conversation step before it.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import logging
import os
import re
from dataclasses import dataclass, field
from typing import Any

import aiohttp
from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from .spec import AgentNode, ApiNode, CollectNode, EndNode, Flow

logger = logging.getLogger("flow_engine")

RESUME_MINUTES = 15  # the config service applies the same limit


@dataclass
class ResumePoint:
    start: str  # the step the call continues at
    then: str | None = None  # after verifying again, go here instead of the verifying step's "next"
    values: dict[str, Any] = field(default_factory=dict)


# ── Keys ─────────────────────────────────────────────────────────────────────


def load_secret(value: str | None) -> bytes | None:
    """RESUME_KEY: 32 random bytes, as base64 or hex (e.g. `openssl rand -base64 32`)."""
    if not value:
        return None
    for decode in (base64.b64decode, bytes.fromhex):
        try:
            raw = decode(value.strip())
        except (binascii.Error, ValueError):
            continue
        if len(raw) == 32:
            return raw
    logger.error("RESUME_KEY must be 32 random bytes, base64 or hex (openssl rand -base64 32); resume is off")
    return None


def _subkey(secret: bytes, purpose: bytes) -> bytes:
    return hmac.new(secret, b"flow-studio-resume:" + purpose, hashlib.sha256).digest()


def caller_key(secret: bytes, caller_number: str) -> str:
    """Where a caller's resume point is filed: an HMAC of the last 10 digits of
    their number ('' for withheld or odd numbers, which never resume)."""
    digits = re.sub(r"\D", "", caller_number or "")
    if len(digits) < 8:
        return ""
    return hmac.new(_subkey(secret, b"key"), digits[-10:].encode(), hashlib.sha256).hexdigest()


def seal(secret: bytes, flow_id: str, point: ResumePoint) -> str:
    plain = json.dumps({"start": point.start, "then": point.then, "values": point.values}, ensure_ascii=False, default=str)
    nonce = os.urandom(12)
    sealed = AESGCM(_subkey(secret, b"seal")).encrypt(nonce, plain.encode(), flow_id.encode())
    return base64.b64encode(nonce + sealed).decode()


def unseal(secret: bytes, flow_id: str, sealed: str) -> ResumePoint | None:
    try:
        raw = base64.b64decode(sealed)
        plain = AESGCM(_subkey(secret, b"seal")).decrypt(raw[:12], raw[12:], flow_id.encode())
        data = json.loads(plain)
        return ResumePoint(start=str(data["start"]), then=data.get("then"), values=dict(data.get("values") or {}))
    except (InvalidTag, ValueError, KeyError, TypeError, binascii.Error):
        return None


# ── Where to resume ──────────────────────────────────────────────────────────


def _talks(flow: Flow, node_id: str | None) -> bool:
    return isinstance(flow.nodes.get(node_id or ""), (AgentNode, CollectNode))


def choose_point(
    flow: Flow,
    path: list[str],
    values: dict[str, Any],
    verified: bool,
    pending_then: str | None = None,
    claimed: ResumePoint | None = None,
) -> ResumePoint | None:
    """Where a call that just dropped should pick up again (None: nowhere).

    path: the steps the call went through; verified: whether the caller had
    been verified; pending_then / claimed: from a resumed call that hadn't got
    as far as the original point yet.
    """
    if path and isinstance(flow.nodes.get(path[-1]), EndNode):
        return None  # it got to the end: nothing to resume
    talking = [i for i, nid in enumerate(path) if _talks(flow, nid)]
    if not talking:
        return claimed  # dropped while being asked "continue or start over?": keep the old point
    resume = path[talking[-1]]
    start, then = resume, None
    if verified:
        verifying = [i for i in range(talking[-1]) if isinstance(flow.nodes.get(path[i]), ApiNode) and flow.nodes[path[i]].verifies]
        if verifying:
            before = [i for i in talking if i < verifying[-1]]
            start, then = (path[before[-1]] if before else flow.start_node), resume
    elif pending_then:
        then = pending_then  # resumed, but hadn't verified again yet
    keep = {k: values[k] for k in flow.settings.resume.keep if values.get(k) not in (None, "")}
    return ResumePoint(start=start, then=then if then != start else None, values=keep)


def usable(flow: Flow, point: ResumePoint) -> bool:
    """The flow may have been republished since: both steps must still exist and talk."""
    return _talks(flow, point.start) and (point.then is None or _talks(flow, point.then))


# ── The config service ───────────────────────────────────────────────────────


class ResumeStore:
    """Saves and claims resume points through the config service's internal API."""

    def __init__(self, url: str | None, token: str | None, secret: bytes | None, timeout: float = 1.5) -> None:
        self.url = (url or "").rstrip("/")
        self.token = token
        self.secret = secret
        self.timeout = aiohttp.ClientTimeout(total=timeout)

    @property
    def enabled(self) -> bool:
        return bool(self.url and self.token and self.secret)

    def _headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self.token}"}

    async def save(
        self, http: aiohttp.ClientSession, flow_id: str, version: int | None, caller_number: str, point: ResumePoint
    ) -> bool:
        key = caller_key(self.secret, caller_number) if self.enabled else ""  # type: ignore[arg-type]
        if not key:
            return False
        body = {"flow_id": flow_id, "flow_version": version, "sealed": seal(self.secret, flow_id, point)}  # type: ignore[arg-type]
        try:
            async with http.put(f"{self.url}/internal/v1/resume/{key}", json=body, headers=self._headers(), timeout=self.timeout) as r:
                if r.status in (200, 201):
                    logger.info("resume point saved", extra={"flow_id": flow_id, "step": point.then or point.start})
                    return True
                logger.warning("config service didn't take the resume point (%s)", r.status)
        except Exception as e:  # never let this break the end of a call
            logger.warning("couldn't save the resume point (%s)", type(e).__name__)
        return False

    async def claim(self, http: aiohttp.ClientSession, flow: Flow, caller_number: str) -> ResumePoint | None:
        """The caller's resume point for this flow, if any (claiming deletes it)."""
        key = caller_key(self.secret, caller_number) if self.enabled else ""  # type: ignore[arg-type]
        if not key:
            return None
        try:
            async with http.post(
                f"{self.url}/internal/v1/resume/{key}/claim",
                json={"flow_id": flow.flow_id},
                headers=self._headers(),
                timeout=self.timeout,
            ) as r:
                if r.status == 404:
                    return None
                if r.status != 200:
                    logger.warning("config service couldn't look up a resume point (%s)", r.status)
                    return None
                data = await r.json()
        except Exception as e:  # a slow lookup must not hold up answering the call
            logger.warning("couldn't look up a resume point (%s)", type(e).__name__)
            return None
        point = unseal(self.secret, flow.flow_id, str(data.get("sealed", "")))  # type: ignore[arg-type]
        if point is None:
            logger.warning("resume point couldn't be opened (RESUME_KEY changed?); starting fresh")
            return None
        if not usable(flow, point):
            logger.info("resume point no longer fits the flow (republished); starting fresh")
            return None
        return point
