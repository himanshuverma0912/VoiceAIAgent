"""Tools: calling a flow's integrations (a bank's REST API or an MCP server).

The config service sends, with each flow, the integrations it uses: where to
call, which of this agent's environment variables holds the credential (always
INTEGRATION_..., never another key of the agent), and each tool's description,
inputs, outputs and rules. A ToolCaller is made per call and:

  - connects to the call's MCP servers while the greeting plays, and keeps one
    connection per server for the whole call;
  - fills a tool's inputs (the flow's own values, plus what the LLM chose);
  - refuses a tool that needs a verified caller before the caller is verified,
    and a tool used more often than its "max_calls";
  - times out after the integration's timeout_ms;
  - records which tool ran, whether it worked and how long it took (for call
    history). What was sent and what came back is never logged or stored.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import time
from collections.abc import Callable, Mapping
from contextlib import AsyncExitStack
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import quote

import aiohttp

logger = logging.getLogger("flow_engine.tools")

AUTH_ENV_RE = re.compile(r"^INTEGRATION_[A-Z0-9_]{1,60}$")
PATH_PARAM_RE = re.compile(r"\{([A-Za-z_][A-Za-z0-9_]*)\}")
MAX_RESULT_CHARS = 4000  # what the LLM gets back from one tool call


class ToolFailed(Exception):
    """A tool call didn't work; the message is safe to show the LLM."""


@dataclass(frozen=True)
class ToolInput:
    name: str
    type: str = "string"
    description: str = ""
    required: bool = False
    enum: tuple[str, ...] = ()


@dataclass(frozen=True)
class ToolDef:
    name: str
    description: str
    inputs: tuple[ToolInput, ...] = ()
    outputs: tuple[str, ...] = ()
    sensitive: bool = False
    locked: tuple[str, ...] = ()
    max_calls: int = 5
    method: str = "POST"
    path: str = ""

    def input(self, name: str) -> ToolInput | None:
        return next((i for i in self.inputs if i.name == name), None)


@dataclass(frozen=True)
class Integration:
    id: str
    kind: str  # "mcp" or "http"
    url: str
    auth_header: str = ""
    auth_env: str = ""
    timeout_ms: int = 5000
    tools: Mapping[str, ToolDef] = field(default_factory=dict)

    def headers(self, env: Mapping[str, str]) -> dict[str, str]:
        if not self.auth_header:
            return {}
        if not AUTH_ENV_RE.match(self.auth_env):
            # The config service checks this too; never send another of the agent's keys.
            logger.error("integration credential variable isn't an INTEGRATION_ variable", extra={"integration": self.id})
            return {}
        value = (env.get(self.auth_env) or "").strip()
        if not value:
            logger.error("integration credential not set in .env", extra={"integration": self.id, "variable": self.auth_env})
            return {}
        return {self.auth_header: value}


def parse_integrations(data: Any) -> dict[str, Integration]:
    """The "integrations" part of the config service's answer. Entries that
    don't look right are left out (their tools then just aren't available)."""
    out: dict[str, Integration] = {}
    if not isinstance(data, dict):
        return out
    for iid, raw in data.items():
        try:
            tools = {}
            for t in raw.get("tools") or []:
                inputs = tuple(
                    ToolInput(
                        name=str(i["name"]),
                        type=str(i.get("type") or "string"),
                        description=str(i.get("description") or ""),
                        required=bool(i.get("required")),
                        enum=tuple(str(e) for e in i.get("enum") or ()),
                    )
                    for i in t.get("inputs") or []
                )
                tools[str(t["name"])] = ToolDef(
                    name=str(t["name"]),
                    description=str(t.get("description") or ""),
                    inputs=inputs,
                    outputs=tuple(str(o) for o in t.get("outputs") or ()),
                    sensitive=bool(t.get("sensitive")),
                    locked=tuple(str(x) for x in t.get("locked") or ()),
                    max_calls=int(t.get("max_calls") or 5),
                    method=str(t.get("method") or "POST").upper(),
                    path=str(t.get("path") or ""),
                )
            kind = str(raw["kind"])
            if kind not in ("mcp", "http"):
                raise ValueError(kind)
            out[str(iid)] = Integration(
                id=str(iid),
                kind=kind,
                url=str(raw["url"]),
                auth_header=str(raw.get("auth_header") or ""),
                auth_env=str(raw.get("auth_env") or ""),
                timeout_ms=int(raw.get("timeout_ms") or 5000),
                tools=tools,
            )
        except (KeyError, TypeError, ValueError, AttributeError):
            logger.error("skipping an integration the agent can't read", extra={"integration": str(iid)})
    return out


def integrations_to_json(integrations: Mapping[str, Integration]) -> dict:
    """The inverse of parse_integrations (for the on-disk flow cache)."""
    out = {}
    for iid, i in integrations.items():
        out[iid] = {
            "kind": i.kind,
            "url": i.url,
            "auth_header": i.auth_header,
            "auth_env": i.auth_env,
            "timeout_ms": i.timeout_ms,
            "tools": [
                {
                    "name": t.name,
                    "description": t.description,
                    "inputs": [
                        {"name": x.name, "type": x.type, "description": x.description, "required": x.required, "enum": list(x.enum)}
                        for x in t.inputs
                    ],
                    "outputs": list(t.outputs),
                    "sensitive": t.sensitive,
                    "locked": list(t.locked),
                    "max_calls": t.max_calls,
                    "method": t.method,
                    "path": t.path,
                }
                for t in i.tools.values()
            ],
        }
    return out


def convert(value: Any, type_: str) -> Any:
    """A value set by the flow is text; give the tool the type it expects."""
    if not isinstance(value, str):
        return value
    text = value.strip()
    try:
        if type_ == "integer":
            return int(float(text))
        if type_ == "number":
            return float(text)
    except ValueError:
        raise ToolFailed(f"'{value}' isn't a number") from None
    if type_ == "boolean":
        return text.lower() in ("true", "yes", "1", "haan", "ha")
    return text


def json_schema(tool: ToolDef, hidden: set[str]) -> dict:
    """The tool's parameters as the LLM sees them: inputs the flow sets are left out."""
    properties: dict[str, Any] = {}
    required: list[str] = []
    for i in tool.inputs:
        if i.name in hidden:
            continue
        prop: dict[str, Any] = {"type": i.type, "description": i.description or i.name}
        if i.enum:
            prop["enum"] = list(i.enum)
        properties[i.name] = prop
        if i.required:
            required.append(i.name)
    return {"type": "object", "properties": properties, "required": required, "additionalProperties": False}


def for_llm(result: dict) -> str:
    text = json.dumps(result, ensure_ascii=False, default=str)
    return text if len(text) <= MAX_RESULT_CHARS else text[:MAX_RESULT_CHARS] + "… (cut short)"


class _MCPConnection:
    """One MCP session for the call, held open by a background task (the MCP
    library's context managers must be entered and left in the same task)."""

    def __init__(self, integration: Integration, env: Mapping[str, str]) -> None:
        self.integration = integration
        self._env = env
        self._ready: asyncio.Future[Any] | None = None
        self._closing = asyncio.Event()
        self._task: asyncio.Task | None = None

    def start(self) -> None:
        if self._task is None:
            self._ready = asyncio.get_running_loop().create_future()
            self._task = asyncio.create_task(self._run(), name=f"mcp-{self.integration.id}")

    async def _run(self) -> None:
        import httpx2
        from mcp import ClientSession
        from mcp.client.streamable_http import streamable_http_client

        assert self._ready is not None
        timeout = self.integration.timeout_ms / 1000
        try:
            async with AsyncExitStack() as stack:
                http = await stack.enter_async_context(
                    httpx2.AsyncClient(
                        headers=self.integration.headers(self._env), timeout=httpx2.Timeout(timeout, read=max(timeout, 30))
                    )
                )
                streams = await stack.enter_async_context(streamable_http_client(url=self.integration.url, http_client=http))
                session = await stack.enter_async_context(ClientSession(streams[0], streams[1]))
                await asyncio.wait_for(session.initialize(), timeout)
                if not self._ready.done():
                    self._ready.set_result(session)
                await self._closing.wait()
        except BaseException as e:  # noqa: BLE001 - reported to whoever waits for the session
            if not self._ready.done():
                self._ready.set_exception(ToolFailed("the service isn't reachable right now"))
                logger.error(
                    "couldn't connect to an MCP integration (%s)", type(e).__name__, extra={"integration": self.integration.id}
                )
            if isinstance(e, asyncio.CancelledError):
                raise

    async def session(self):
        self.start()
        assert self._ready is not None
        return await asyncio.shield(self._ready)

    async def aclose(self) -> None:
        self._closing.set()
        if self._task:
            try:
                await asyncio.wait_for(self._task, 3)
            except (asyncio.TimeoutError, Exception):  # noqa: BLE001 - closing is best effort
                self._task.cancel()
        if self._ready and self._ready.done() and not self._ready.cancelled():
            self._ready.exception()  # mark it retrieved


def _mcp_result(result: Any) -> dict:
    if getattr(result, "is_error", False):
        texts = [getattr(c, "text", "") for c in getattr(result, "content", None) or []]
        raise ToolFailed((" ".join(t for t in texts if t) or "the service reported an error")[:300])
    structured = getattr(result, "structured_content", None)
    if isinstance(structured, dict):
        return structured
    texts = [getattr(c, "text", "") for c in getattr(result, "content", None) or [] if getattr(c, "text", None)]
    joined = "\n".join(texts)
    try:
        parsed = json.loads(joined)
        return parsed if isinstance(parsed, dict) else {"result": parsed}
    except ValueError:
        return {"result": joined}


@dataclass
class ToolUseRecord:
    tool: str  # "<integration>.<tool>"
    ok: bool
    ms: int


class ToolCaller:
    """Calls a flow's tools during one call. See the module docstring."""

    def __init__(
        self,
        integrations: Mapping[str, Integration],
        *,
        http: aiohttp.ClientSession | None = None,
        env: Mapping[str, str] | None = None,
        on_used: Callable[[ToolUseRecord], None] | None = None,
    ) -> None:
        self.integrations = dict(integrations)
        self._http = http
        self._own_http: aiohttp.ClientSession | None = None
        self._env = os.environ if env is None else env
        self._on_used = on_used
        self._mcp: dict[str, _MCPConnection] = {}
        self._counts: dict[str, int] = {}
        self.verified = False

    def tool(self, ref: str) -> tuple[Integration, ToolDef] | None:
        iid, _, name = ref.partition(".")
        integration = self.integrations.get(iid)
        tool = integration.tools.get(name) if integration else None
        return (integration, tool) if integration and tool else None

    def warm_up(self) -> None:
        """Connect to the MCP servers now (while the greeting plays)."""
        for i in self.integrations.values():
            if i.kind == "mcp":
                self._connection(i).start()

    def _connection(self, i: Integration) -> _MCPConnection:
        if i.id not in self._mcp:
            self._mcp[i.id] = _MCPConnection(i, self._env)
        return self._mcp[i.id]

    async def call(self, ref: str, arguments: Mapping[str, Any]) -> dict:
        """Call a tool. Raises ToolFailed (with a message for the LLM)."""
        found = self.tool(ref)
        if found is None:
            raise ToolFailed("this tool isn't available right now")
        integration, tool = found
        if tool.sensitive and not self.verified:
            logger.warning("tool needs a verified caller; refused", extra={"tool": ref})
            raise ToolFailed("the caller hasn't been verified yet, so this can't be checked")
        if self._counts.get(ref, 0) >= tool.max_calls:
            raise ToolFailed("this has been checked as many times as allowed on one call; offer a call back instead")
        self._counts[ref] = self._counts.get(ref, 0) + 1

        args: dict[str, Any] = {}
        for name, value in arguments.items():
            spec = tool.input(name)
            if spec is None or value is None or value == "":
                continue
            args[name] = convert(value, spec.type)
        missing = [i.name for i in tool.inputs if i.required and i.name not in args]

        started = time.monotonic()
        ok = False
        try:
            if missing:
                raise ToolFailed(f"missing {', '.join(missing)}")
            timeout = integration.timeout_ms / 1000
            if integration.kind == "mcp":
                result = await asyncio.wait_for(self._call_mcp(integration, tool, args), timeout)
            else:
                result = await asyncio.wait_for(self._call_http(integration, tool, args), timeout)
            ok = True
            return result
        except asyncio.TimeoutError:
            raise ToolFailed("the service took too long to answer") from None
        except ToolFailed:
            raise
        except Exception as e:  # noqa: BLE001 - never let a tool break the call
            logger.error("tool call failed (%s)", type(e).__name__, extra={"tool": ref})
            raise ToolFailed("the service isn't reachable right now") from None
        finally:
            ms = int((time.monotonic() - started) * 1000)
            # Which tool, whether it worked, how long: never the inputs or the answer.
            logger.info("tool called", extra={"tool": ref, "ok": ok, "ms": ms})
            if self._on_used:
                try:
                    self._on_used(ToolUseRecord(ref, ok, ms))
                except Exception:  # noqa: BLE001
                    logger.exception("recording a tool call failed")

    async def _call_mcp(self, integration: Integration, tool: ToolDef, args: dict) -> dict:
        session = await self._connection(integration).session()
        return _mcp_result(await session.call_tool(tool.name, args))

    async def _call_http(self, integration: Integration, tool: ToolDef, args: dict) -> dict:
        args = dict(args)

        def fill(m: re.Match) -> str:
            value = args.pop(m.group(1), None)
            if value is None:
                raise ToolFailed(f"missing {m.group(1)}")
            return quote(str(value), safe="")

        url = integration.url.rstrip("/") + PATH_PARAM_RE.sub(fill, tool.path or "/" + tool.name)
        http = self._http
        if http is None:
            if self._own_http is None:
                self._own_http = aiohttp.ClientSession()
            http = self._own_http
        headers = integration.headers(self._env)
        if tool.method == "GET":
            request = http.get(url, params={k: str(v) for k, v in args.items()}, headers=headers)
        else:
            request = http.post(url, json=args, headers=headers)
        async with request as resp:
            text = await resp.text()
            try:
                data = json.loads(text) if text else {}
            except ValueError:
                data = {"result": text[:MAX_RESULT_CHARS]}
            if resp.status >= 400:
                message = data.get("error") if isinstance(data, dict) else None
                raise ToolFailed(str(message or f"the service answered {resp.status}")[:300])
            return data if isinstance(data, dict) else {"result": data}

    async def aclose(self) -> None:
        for c in self._mcp.values():
            await c.aclose()
        if self._own_http is not None:
            await self._own_http.close()
