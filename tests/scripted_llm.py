"""A fake LLM for testing the flow engine without network access or API keys.

The test drives it through the caller's messages:
  - "CALL <tool> {json}"  -> the LLM calls that tool (it must be available at this step)
  - anything else         -> the LLM replies "[<step>] ok", where <step> is the
                             flow node whose instructions it was given, so tests
                             can see which node is speaking.
Every request is recorded in .requests (instructions + tool names) for assertions.
"""

from __future__ import annotations

import json
import re
import uuid

from livekit.agents import llm
from livekit.agents.types import DEFAULT_API_CONNECT_OPTIONS

STEP_RE = re.compile(r"Current step \(([a-z0-9_]+)\)")


def _tool_name(tool) -> str:
    return getattr(tool, "id", None) or getattr(getattr(tool, "info", None), "name", "?")


class ScriptedLLM(llm.LLM):
    def __init__(self) -> None:
        super().__init__()
        self.requests: list[dict] = []

    @property
    def model(self) -> str:
        return "scripted"

    def chat(self, *, chat_ctx, tools=None, conn_options=DEFAULT_API_CONNECT_OPTIONS, tool_choice=None, **kwargs):
        stream = _ScriptedStream(self, chat_ctx=chat_ctx, tools=tools or [], conn_options=conn_options)
        stream.forced_tool = (
            tool_choice["function"]["name"] if isinstance(tool_choice, dict) and "function" in tool_choice else None
        )
        return stream


class _ScriptedStream(llm.LLMStream):
    forced_tool: str | None = None

    async def _run(self) -> None:
        if self.forced_tool:
            # Like a real LLM given a named tool_choice: it must call that tool.
            self._llm.requests.append({"step": "forced", "instructions": "", "tools": [self.forced_tool]})
            self._call(self.forced_tool, "{}")
            return
        await self._scripted()

    def _call(self, name: str, args: str) -> None:
        self._event_ch.send_nowait(
            llm.ChatChunk(
                id=str(uuid.uuid4()),
                delta=llm.ChoiceDelta(
                    role="assistant",
                    tool_calls=[llm.FunctionToolCall(name=name, arguments=args, call_id=f"call_{uuid.uuid4().hex[:8]}")],
                ),
            )
        )

    async def _scripted(self) -> None:
        items = self._chat_ctx.items
        instructions = "\n".join(
            i.text_content or "" for i in items if i.type == "message" and i.role in ("system", "developer")
        )
        tools = {_tool_name(t): t for t in self._tools}
        step = (STEP_RE.findall(instructions) or ["task"])[-1]
        self._llm.requests.append({"step": step, "instructions": instructions, "tools": sorted(tools)})

        last = items[-1] if items else None
        if last is not None and last.type == "message" and last.role == "user":
            text = (last.text_content or "").strip()
            if text.startswith("CALL "):
                name, _, args = text[len("CALL ") :].partition(" ")
                if name not in tools:
                    self._say(f"[{step}] NO TOOL {name}; available: {sorted(tools)}")
                    return
                self._call(name, args or "{}")
                return
        self._say(f"[{step}] ok")

    def _say(self, text: str) -> None:
        self._event_ch.send_nowait(
            llm.ChatChunk(id=str(uuid.uuid4()), delta=llm.ChoiceDelta(role="assistant", content=text))
        )


def call(tool: str, **args) -> str:
    """Build the caller message that makes the ScriptedLLM call a tool."""
    return f"CALL {tool} {json.dumps(args, ensure_ascii=False)}"
