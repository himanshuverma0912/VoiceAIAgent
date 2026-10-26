"""Runs any flow (see spec.py) on LiveKit Agents.

Nothing here knows about a particular project. Each node in the flow file
becomes a LiveKit agent when the conversation reaches it:

    agent node    -> FlowAgent: the node's instructions, plus one tool per
                     transition. When the LLM calls a transition tool, the
                     captured values are saved and the call is handed off to
                     the next node (LiveKit agent handoff).
    collect node  -> CollectAgent: runs a LiveKit TaskGroup with one task per
                     field (LiveKit's prebuilt tasks for built-in fields, a
                     small custom task for the rest), saves the answers, then
                     hands off to the node in "next".
    end node      -> EndAgent: says the node's final words plus the goodbye,
                     then hangs up (always; it doesn't wait for the caller).

Every node's instructions are built the same way: persona, guardrails,
values collected so far, then the node's own instructions, with
{{variables}} filled in.

Docs this follows:
  handoffs   https://docs.livekit.io/agents/logic/agents-handoffs/
  tasks      https://docs.livekit.io/agents/logic/tasks/
  tools      https://docs.livekit.io/agents/logic/tools/definition/
"""

from __future__ import annotations

import logging
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any
from zoneinfo import ZoneInfo

from livekit.agents import NOT_GIVEN, Agent, AgentTask, RunContext, function_tool, llm
from livekit.agents.beta.tools import EndCallTool
from livekit.agents.beta.workflows import (
    GetAddressTask,
    GetDOBTask,
    GetDtmfTask,
    GetEmailTask,
    GetNameTask,
    GetPhoneNumberTask,
    TaskGroup,
)

from .spec import (
    VARIABLE_RE,
    AgentNode,
    BuiltinDigitsField,
    BuiltinNameField,
    BuiltinSimpleField,
    CollectNode,
    CustomField,
    EndNode,
    Flow,
)

logger = logging.getLogger("flow_engine")

# When the caller wants out, at any step (including in the middle of a question).
EXIT_RULE = (
    "If the caller says they no longer need help (for example their problem is already "
    "solved) or clearly wants to end the call, use the end_call tool right away instead "
    "of continuing."
)
EXIT_GOODBYE = (
    "Acknowledge what the caller said, thank them briefly, in the language they are "
    "speaking, and say goodbye."
)

IST = ZoneInfo("Asia/Kolkata")
NOT_PROVIDED = "(not provided)"

# Builds the end-call tool(s). Tests swap this for a stub that doesn't need a room.
EndCallFactory = Callable[[str, str], list[llm.Tool | llm.Toolset]]


def default_end_call(
    goodbye: str, extra_description: str, on_called: Callable[[], None] | None = None
) -> list[llm.Tool | llm.Toolset]:
    """on_called runs when the agent decides to hang up (call history uses it to
    tell "the agent ended the call" apart from "the caller hung up")."""

    async def called(_ev: Any) -> None:
        if on_called:
            try:
                on_called()
            except Exception:
                logger.exception("end-call hook failed")

    # delete_room=True closes the LiveKit room, which hangs up the phone call.
    return [
        EndCallTool(
            extra_description=extra_description, delete_room=True, end_instructions=goodbye, on_tool_called=called
        )
    ]


@dataclass
class CallState:
    """Everything the engine knows about one call. Lives in memory only and is
    gone when the call ends (nothing is stored)."""

    flow: Flow
    caller_number: str = ""
    dialed_number: str = ""
    values: dict[str, Any] = field(default_factory=dict)
    path: list[str] = field(default_factory=list)
    greeted: bool = False
    end_call: EndCallFactory = default_end_call
    # Called with the node id whenever a step starts (the agent publishes it to
    # the room so Flow Studio can highlight the current step in a test call).
    on_node: Callable[[str], None] | None = None

    def variables(self) -> dict[str, str]:
        now = datetime.now(IST)
        out = {
            "caller_number": self.caller_number or NOT_PROVIDED,
            "dialed_number": self.dialed_number or NOT_PROVIDED,
            "now_ist": now.strftime("%A, %B %d, %Y at %I:%M %p IST"),
            "today_ist": now.strftime("%A, %B %d, %Y"),
        }
        out.update({k: str(v) for k, v in self.values.items() if v not in (None, "")})
        return out

    def render(self, text: str) -> str:
        """Fill in {{variables}}; anything not collected yet reads '(not provided)'."""
        known = self.variables()
        return VARIABLE_RE.sub(lambda m: known.get(m.group(1), NOT_PROVIDED), text)


# ── Instructions ──────────────────────────────────────────────────────────────


def compose_instructions(state: CallState, node_id: str, body: str) -> str:
    flow = state.flow
    parts = [state.render(flow.persona).strip()]
    if flow.guardrails:
        parts.append(
            "Rules you must always follow:\n" + "\n".join(f"- {state.render(g)}" for g in flow.guardrails)
        )
    if flow.allow_end_call_anywhere:
        parts.append(EXIT_RULE)
    collected = {k: v for k, v in state.values.items() if v not in (None, "")}
    if collected:
        parts.append(
            "Information already collected in this call (don't ask for it again):\n"
            + "\n".join(f"- {k}: {v}" for k, v in collected.items())
        )
    parts.append(f"Current step ({node_id}):\n{state.render(body).strip()}")
    return "\n\n".join(parts)


def _json_type(t: str) -> str:
    return {"string": "string", "number": "number", "integer": "integer", "boolean": "boolean"}[t]


# ── Building the agent for a node ─────────────────────────────────────────────


def build_node_agent(state: CallState, node_id: str, chat_ctx: llm.ChatContext | None = None) -> Agent:
    """Create the LiveKit agent for one node of the flow."""
    node = state.flow.nodes[node_id]
    if isinstance(node, AgentNode):
        return FlowAgent(state, node_id, node, chat_ctx)
    if isinstance(node, CollectNode):
        return CollectAgent(state, node_id, node, chat_ctx)
    if isinstance(node, EndNode):
        return EndAgent(state, node_id, node, chat_ctx)
    raise TypeError(f"unsupported node type: {type(node).__name__}")


def start_agent(state: CallState) -> Agent:
    return build_node_agent(state, state.flow.start_node)


def exit_tools(state: CallState) -> list[llm.Tool | llm.Toolset]:
    """The end_call tool offered at every step, if the flow allows ending anywhere."""
    if not state.flow.allow_end_call_anywhere:
        return []
    return state.end_call(EXIT_GOODBYE, "Use it when the caller no longer needs help or wants to end the call.")


def _handoff_ctx(agent: Agent) -> llm.ChatContext:
    # The next node hears the conversation so far, but not the previous node's instructions.
    return agent.chat_ctx.copy(exclude_instructions=True)


class _NodeMixin:
    """Shared behaviour: the greeting on the very first node, and logging."""

    state: CallState
    node_id: str

    async def _greet_once(self) -> None:
        state = self.state
        if not state.greeted:
            state.greeted = True
            if state.flow.greeting:
                # The caller can't interrupt the greeting, so they always hear it fully.
                await self.session.say(state.render(state.flow.greeting), allow_interruptions=False)  # type: ignore[attr-defined]

    def _entered(self) -> None:
        self.state.path.append(self.node_id)
        logger.info("flow node entered", extra={"flow_id": self.state.flow.flow_id, "node": self.node_id})
        if self.state.on_node:
            try:
                self.state.on_node(self.node_id)
            except Exception:  # showing the step is a nicety; never let it break a call
                logger.exception("couldn't report the current step")


class FlowAgent(_NodeMixin, Agent):
    def __init__(
        self, state: CallState, node_id: str, node: AgentNode, chat_ctx: llm.ChatContext | None
    ) -> None:
        self.state, self.node_id, self.node = state, node_id, node
        tools: list[llm.Tool | llm.Toolset] = [self._transition_tool(t) for t in node.transitions]
        if state.flow.allow_end_call_anywhere:
            tools += exit_tools(state)
        body = node.instructions
        if node.transitions:
            body += "\n\nWhen to move on: as soon as one of these conditions is met, call its tool. " \
                    "Don't tell the caller you are moving to another step.\n" + "\n".join(
                        f"- {t.tool_name}: {t.when}" for t in node.transitions
                    )
        super().__init__(
            instructions=compose_instructions(state, node_id, body),
            tools=tools,
            chat_ctx=chat_ctx,
        )

    def _transition_tool(self, transition) -> llm.Tool:
        properties = {}
        required = []
        for name, cap in transition.capture.items():
            prop: dict[str, Any] = {"type": _json_type(cap.type), "description": cap.description}
            if cap.enum:
                prop["enum"] = cap.enum
            properties[name] = prop
            if cap.required:
                required.append(name)
        schema = {
            "type": "function",
            "name": transition.tool_name,
            "description": f"Move the conversation on. Call this when: {transition.when}",
            "parameters": {
                "type": "object",
                "properties": properties,
                "required": required,
                "additionalProperties": False,
            },
        }
        state, target = self.state, transition.to

        @function_tool(raw_schema=schema)
        async def _go(raw_arguments: dict[str, object], context: RunContext) -> Agent:
            state.values.update({k: v for k, v in raw_arguments.items() if k in transition.capture})
            logger.info("flow transition", extra={"from": self.node_id, "to": target})
            return build_node_agent(state, target, _handoff_ctx(self))

        return _go

    async def on_enter(self) -> None:
        self._entered()
        await self._greet_once()
        if self.node.on_enter == "speak":
            self.session.generate_reply()


class EndAgent(_NodeMixin, Agent):
    """Says the node's final words and hangs up, every time.

    Ending doesn't depend on the LLM deciding to call end_call: on entering the
    node, end_call is forced. Its result tells the LLM what to say (the node's
    instructions plus the goodbye); the call is hung up once that's spoken.
    (Waiting for the caller to talk after a goodbye left calls open: callers
    just wait silently for the line to drop.)
    """

    def __init__(self, state: CallState, node_id: str, node: EndNode, chat_ctx: llm.ChatContext | None) -> None:
        self.state, self.node_id, self.node = state, node_id, node
        final_words = (
            f"{state.render(node.instructions).strip()}\n"
            f"{state.render(node.goodbye).strip()}\n"
            "Say this as one short final message; the call ends right after it."
        )
        super().__init__(
            instructions=compose_instructions(state, node_id, node.instructions),
            tools=state.end_call(final_words, "Ends the call after the final message."),
            chat_ctx=chat_ctx,
        )

    async def on_enter(self) -> None:
        self._entered()
        await self._greet_once()
        self.session.generate_reply(tool_choice={"type": "function", "function": {"name": "end_call"}})


# ── Collect node ──────────────────────────────────────────────────────────────


class CustomFieldTask(AgentTask[Any]):
    """Asks for one custom field and returns the caller's answer."""

    def __init__(self, state: CallState, f: CustomField, chat_ctx: llm.ChatContext | None) -> None:
        self.field_def = f
        value_schema: dict[str, Any] = {"type": _json_type(f.type), "description": f.description}
        if f.enum:
            value_schema["enum"] = f.enum
        schema = {
            "type": "function",
            "name": f"record_{f.name}",
            "description": f"Record the caller's answer for: {f.description}. "
            "Call it once the caller has clearly told you.",
            "parameters": {
                "type": "object",
                "properties": {"value": value_schema},
                "required": ["value"],
                "additionalProperties": False,
            },
        }

        @function_tool(raw_schema=schema)
        async def _record(raw_arguments: dict[str, object], context: RunContext) -> None:
            self.complete(raw_arguments["value"])

        extra = f"\n{state.render(f.instructions)}" if f.instructions else ""
        exit_rule = f"\n{EXIT_RULE}" if state.flow.allow_end_call_anywhere else ""
        super().__init__(
            instructions=(
                f"Ask the caller for: {f.description}.{extra}\n"
                "Ask one short question. If the answer is unclear, ask again briefly. "
                f"When you have it, call record_{f.name}.{exit_rule}"
            ),
            chat_ctx=chat_ctx,
            tools=[_record, *exit_tools(state)],
        )

    async def on_enter(self) -> None:
        self.session.generate_reply()


class _DigitsTaskWithExit(GetDtmfTask):
    """LiveKit's keypad-digits task doesn't take extra tools, so add end_call on entry."""

    def __init__(self, *, extra_tools: list, **kwargs) -> None:
        super().__init__(**kwargs)
        self._extra_tools = extra_tools

    async def on_enter(self) -> None:
        if self._extra_tools:
            await self.update_tools([*self.tools, *self._extra_tools])
        await super().on_enter()


def _builtin_task(state: CallState, f, chat_ctx: llm.ChatContext | None) -> AgentTask:
    extra = state.render(f.instructions) if f.instructions else ""
    tools = exit_tools(state)
    if tools:
        # LiveKit's built-in tasks have their own instructions, so tell them about ending too.
        extra = f"{extra}\n{EXIT_RULE}".strip()
    if isinstance(f, BuiltinNameField):
        return GetNameTask(
            first_name=True,
            last_name=f.options.last_name,
            verify_spelling=f.options.verify_spelling,
            extra_instructions=extra,
            chat_ctx=chat_ctx,
            tools=tools,
        )
    if isinstance(f, BuiltinDigitsField):
        return _DigitsTaskWithExit(
            num_digits=f.options.num_digits,
            ask_for_confirmation=f.options.ask_for_confirmation,
            # NOT_GIVEN, not None: the task would otherwise append the word "None".
            extra_instructions=extra or NOT_GIVEN,
            chat_ctx=chat_ctx,
            extra_tools=tools,
        )
    task_cls = {
        "phone": GetPhoneNumberTask,
        "email": GetEmailTask,
        "address": GetAddressTask,
        "date_of_birth": GetDOBTask,
    }[f.builtin]
    return task_cls(extra_instructions=extra, chat_ctx=chat_ctx, tools=tools)


def _result_value(f, result: Any) -> Any:
    """Turn a prebuilt task's result object into a plain value for {{variables}}."""
    if isinstance(result, Exception):
        return None
    if isinstance(f, CustomField):
        return result
    if isinstance(f, BuiltinNameField):
        parts = [getattr(result, k, None) for k in ("first_name", "middle_name", "last_name")]
        return " ".join(p for p in parts if p)
    if isinstance(f, BuiltinDigitsField):
        # LiveKit returns the digits space-separated ("4 3 2 1"); store them as "4321".
        digits = getattr(result, "user_input", None)
        return digits.replace(" ", "") if isinstance(digits, str) else digits
    attr = {
        "phone": "phone_number",
        "email": "email_address",
        "address": "address",
        "date_of_birth": "date_of_birth",
    }[f.builtin]
    value = getattr(result, attr, None)
    return value.isoformat() if hasattr(value, "isoformat") else value


class CollectAgent(_NodeMixin, Agent):
    """Collects every field of a collect node, then hands off to its next node."""

    def __init__(self, state: CallState, node_id: str, node: CollectNode, chat_ctx: llm.ChatContext | None) -> None:
        self.state, self.node_id, self.node = state, node_id, node
        body = node.instructions or "Collect the details the next questions ask for."
        super().__init__(instructions=compose_instructions(state, node_id, body), chat_ctx=chat_ctx)

    def _make_task(self, f) -> Callable[[], AgentTask]:
        def factory() -> AgentTask:
            # Field names only, never the caller's answers (nothing said is logged).
            logger.info("flow field asked", extra={"node": self.node_id, "field": f.name})
            ctx = self.chat_ctx.copy(exclude_instructions=True)
            if isinstance(f, CustomField):
                return CustomFieldTask(self.state, f, ctx)
            return _builtin_task(self.state, f, ctx)

        return factory

    async def on_enter(self) -> None:
        self._entered()
        await self._greet_once()
        if self.node.instructions:
            # Introduce the step ("I need a few details") before the first question.
            await self.session.generate_reply(
                instructions=self.state.render(self.node.instructions)
                + "\nSay only the introduction now; the questions come next."
            )

        group = TaskGroup(
            chat_ctx=self.chat_ctx.copy(exclude_instructions=True),
            # Keep the full Q&A rather than spending an extra LLM call to summarise it;
            # the collected values are passed on explicitly anyway.
            summarize_chat_ctx=False,
            return_exceptions=True,
            on_task_completed=self._on_field_done,
        )
        for f in self.node.fields:
            group.add(self._make_task(f), id=f.name, description=self._describe(f))
        result = await group

        if self.session._is_closing():
            # The caller hung up (or the call ended) while this step was still running.
            logger.info("flow call ended", extra={"node": self.node_id})
            return

        for f in self.node.fields:
            self.state.values[f.name] = _result_value(f, result.task_results.get(f.name))
        logger.info("flow transition", extra={"from": self.node_id, "to": self.node.next})
        self.session.update_agent(build_node_agent(self.state, self.node.next, _handoff_ctx(self)))

    async def _on_field_done(self, ev) -> None:
        ok = not isinstance(ev.result, Exception)
        logger.info(
            "flow field collected" if ok else "flow field not collected",
            extra={"node": self.node_id, "field": ev.task_id},
        )

    @staticmethod
    def _describe(f) -> str:
        if isinstance(f, CustomField):
            return f.description
        return {
            "name": "the caller's name",
            "phone": "the caller's phone number",
            "email": "the caller's email address",
            "address": "the caller's address",
            "date_of_birth": "the caller's date of birth",
            "digits": "digits entered on the keypad",
        }[f.builtin]
