"""Which flow a call runs: asked from the config service by the dialled number.

At the start of every call the agent asks the config service
(GET /internal/v1/resolve?number=...) which published flow version the dialled
number runs. Publishing a change in the config service therefore takes effect
on the next call, without a redeploy.

The answer is cached (in memory, and on local disk so new job processes have
it too) with its ETag, so a repeat call usually gets a tiny "304 Not Modified".
Nothing about the call is stored: the cache holds only flow configuration.

If the service can't answer, the call still goes ahead:
  - service down, slow (> timeout) or returns a broken flow
        -> the last flow it gave for this number (cache), else the fallback flow
  - number not mapped (404), or no config service configured
        -> the fallback flow (FLOW_FILE in .env)
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import os
from dataclasses import dataclass
from pathlib import Path

import aiohttp

from .spec import Flow, FlowError, parse_flow

logger = logging.getLogger("flow-source")

# Where a flow came from, as logged with each call.
FROM_SERVICE = "config-service"
FROM_CACHE = "config-service-cache"
FROM_FILE = "fallback-file"


@dataclass(frozen=True)
class ResolvedFlow:
    flow: Flow
    source: str  # FROM_SERVICE, FROM_CACHE or FROM_FILE
    version: int | None = None  # published version; None for the fallback file


@dataclass(frozen=True)
class _Entry:
    etag: str
    version: int
    flow: Flow


class FlowSource:
    def __init__(
        self,
        *,
        fallback: Flow,
        url: str | None = None,
        token: str | None = None,
        timeout: float = 2.0,
        cache_dir: str | Path | None = None,
    ) -> None:
        if url and not token:
            raise RuntimeError("CONFIG_SERVICE_URL is set but CONFIG_SERVICE_TOKEN is missing from .env")
        self.fallback = fallback
        self.url = url.rstrip("/") if url else None
        self._token = token
        self._timeout = aiohttp.ClientTimeout(total=timeout)
        self._cache_dir = Path(cache_dir) if cache_dir else None
        self._memory: dict[str, _Entry] = {}

    async def flow_for(self, number: str, session: aiohttp.ClientSession) -> ResolvedFlow:
        """The flow for a call to `number` (the dialled number, e.g. sip.trunkPhoneNumber)."""
        if not self.url:
            return ResolvedFlow(self.fallback, FROM_FILE)
        if not number:
            logger.warning("no dialled number on this call; using the fallback flow")
            return ResolvedFlow(self.fallback, FROM_FILE)

        cached = self._memory.get(number) or self._read_disk(number)
        headers = {"Authorization": f"Bearer {self._token}"}
        if cached:
            headers["If-None-Match"] = cached.etag
        try:
            async with session.get(
                f"{self.url}/internal/v1/resolve",
                params={"number": number},
                headers=headers,
                timeout=self._timeout,
            ) as resp:
                if resp.status == 304 and cached:
                    self._memory[number] = cached
                    return ResolvedFlow(cached.flow, FROM_SERVICE, cached.version)
                if resp.status == 404:
                    self._forget(number)
                    logger.warning("number isn't mapped to a flow in the config service; using the fallback flow")
                    return ResolvedFlow(self.fallback, FROM_FILE)
                if resp.status != 200:
                    logger.error("config service answered %s", resp.status)
                    return self._last_known(cached)
                body = await resp.text()
                etag = resp.headers.get("ETag", "")
        except (aiohttp.ClientError, asyncio.TimeoutError) as e:
            logger.error("config service not reachable (%s)", type(e).__name__)
            return self._last_known(cached)

        try:
            data = json.loads(body)
            flow = parse_flow(data["spec"])
            entry = _Entry(etag=etag, version=int(data["version"]), flow=flow)
        except FlowError as e:
            # The service validates before publishing, so this means the two
            # validators disagree (or a newer rule): keep calls on the last good flow.
            logger.error("config service sent a flow this agent can't run:\n%s", e)
            return self._last_known(cached)
        except (ValueError, KeyError, TypeError) as e:
            logger.error("config service sent an unexpected answer (%s)", type(e).__name__)
            return self._last_known(cached)

        self._memory[number] = entry
        self._write_disk(number, etag, entry.version, data["spec"])
        return ResolvedFlow(flow, FROM_SERVICE, entry.version)

    def _last_known(self, cached: _Entry | None) -> ResolvedFlow:
        if cached:
            return ResolvedFlow(cached.flow, FROM_CACHE, cached.version)
        return ResolvedFlow(self.fallback, FROM_FILE)

    # ── Disk cache: one small JSON file per dialled number ────────────────────

    def _path(self, number: str) -> Path | None:
        if not self._cache_dir:
            return None
        return self._cache_dir / (hashlib.sha256(number.encode()).hexdigest()[:24] + ".json")

    def _read_disk(self, number: str) -> _Entry | None:
        path = self._path(number)
        if not path or not path.exists():
            return None
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
            return _Entry(etag=data["etag"], version=int(data["version"]), flow=parse_flow(data["spec"]))
        except (OSError, ValueError, KeyError, TypeError, FlowError):
            return None  # unreadable or outdated: ignore it, it's only a cache

    def _write_disk(self, number: str, etag: str, version: int, spec: dict) -> None:
        path = self._path(number)
        if not path:
            return
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            tmp = path.with_suffix(f".{os.getpid()}.tmp")
            tmp.write_text(json.dumps({"etag": etag, "version": version, "spec": spec}, ensure_ascii=False), encoding="utf-8")
            os.replace(tmp, path)  # atomic: other processes never see half a file
        except OSError as e:
            logger.warning("couldn't write the flow cache (%s)", e)

    def _forget(self, number: str) -> None:
        self._memory.pop(number, None)
        path = self._path(number)
        if path:
            path.unlink(missing_ok=True)
