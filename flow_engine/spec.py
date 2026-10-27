"""Flow format (schema version 1).

A *flow* describes one project's conversation as a graph of nodes. It is pure
data: the same flow engine runs every flow, so a new project never needs code
changes. Flows are published in the Go config service (config-service/), which
checks them with a port of this module; flows/ holds the fallback and examples.

Node types
----------
agent    A conversation step with its own instructions. It leaves through
         "transitions": the LLM moves the caller on when a transition's "when"
         condition is met, optionally capturing values (e.g. a category).
collect  Collects named fields from the caller, one after another, then moves
         to "next". Fields are either built-in (name, phone, email, address,
         date_of_birth, digits) or custom (described in plain words). The
         caller can go back and correct an earlier answer.
api      Calls one tool of an integration (a bank API or MCP server) without
         the LLM deciding anything: fills its inputs from {{variables}},
         optionally says a short line while waiting, keeps the outputs listed
         in "save" as variables, then moves to "next" (or "on_error" if the
         call fails). "verifies": true marks a step that checks who the caller
         is; tools that need a verified caller can only be used after it.
end      Final step: says its final words and the goodbye, then hangs up.
         It doesn't wait for the caller, so handle "anything else?" in the
         node before it.

Tools
-----
Integrations are registered in the config service (URL, credentials, which
workspaces may use them, which tools). A flow refers to a tool as
"<integration>.<tool>", e.g. "demo_bank.get_card_balance":
  - the flow's "tools" are offered on every conversation (agent) step;
  - an agent node's "tools" are offered on that step only;
  - "inputs" given in the flow are filled from {{variables}} and hidden from
    the LLM, so it can't change them (e.g. the verified card digits).
Which tools exist, and which need a verified caller, is checked by the config
service against its integrations list; this module checks the shape.

Variables
---------
Text fields can use {{variable}}. Built-ins: caller_number, dialed_number,
now_ist, today_ist. Every captured or collected value is also a variable,
named after the field (e.g. {{issue_summary}}, {{city}}), and so is every
output an API step saves (e.g. {{minimum_due}}).

Check a flow file from the command line:
    python -m flow_engine flows/complaint-helpline.json
"""

from __future__ import annotations

import json
import re
import sys
from collections import deque
from pathlib import Path
from typing import Annotated, Literal, Union

from pydantic import BaseModel, ConfigDict, Discriminator, Field, Tag, ValidationError

SCHEMA_VERSION = 1

BUILTIN_VARIABLES = {
    "caller_number": "The caller's phone number (inbound) or the number dialled (outbound)",
    "dialed_number": "The number the caller dialled (your Vobiz number)",
    "now_ist": "Current date and time in IST, e.g. 'Saturday, October 24, 2026 at 11:05 PM IST'",
    "today_ist": "Today's date in IST, e.g. 'Saturday, October 24, 2026'",
}

# Lower-case identifiers: node ids, field names, transition names.
IDENTIFIER_PATTERN = r"^[a-z][a-z0-9_]{0,47}$"
IDENTIFIER_MESSAGE = "must be lower-case letters, digits and _ , starting with a letter (max 48 characters)"
Identifier = Annotated[str, Field(pattern=IDENTIFIER_PATTERN)]
VARIABLE_RE = re.compile(r"\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}")

# A tool of an integration: "<integration>.<tool>". MCP tool names may use
# capitals and "-", so the tool part allows them.
TOOL_PATTERN = r"^[a-z][a-z0-9_]{0,31}\.[A-Za-z0-9_-]{1,64}$"
TOOL_MESSAGE = "must be '<integration>.<tool>', e.g. 'demo_bank.get_card_balance'"


class _Strict(BaseModel):
    # Unknown keys are errors, so a typo like "transitons" is caught instead of ignored.
    # strict: JSON types must match exactly ("4" is not a number, "true" is not a
    # boolean), the same rules the Go config service applies.
    model_config = ConfigDict(extra="forbid", strict=True)


# ── Values captured on a transition or collected by a custom field ────────────


class CaptureField(_Strict):
    type: Literal["string", "number", "integer", "boolean"] = "string"
    description: str = Field(min_length=1, description="What the value is, in plain words for the LLM")
    enum: list[str] | None = Field(default=None, min_length=1, description="Allowed values (string fields only)")
    required: bool = True


class Transition(_Strict):
    to: Identifier = Field(description="Id of the node to move to")
    when: str = Field(min_length=1, description="When to move on, in plain words for the LLM")
    name: Identifier | None = Field(
        default=None, description="Tool name the LLM sees; defaults to 'go_to_<to>'"
    )
    capture: dict[Identifier, CaptureField] = Field(
        default_factory=dict, description="Values the LLM must fill in when taking this transition"
    )

    @property
    def tool_name(self) -> str:
        return self.name or f"go_to_{self.to}"


class ToolUse(_Strict):
    tool: str = Field(pattern=TOOL_PATTERN, description="'<integration>.<tool>'")
    inputs: dict[str, str] = Field(
        default_factory=dict,
        description="Inputs set by the flow (text with {{variables}}); the LLM can't see or change them",
    )

    @property
    def integration(self) -> str:
        return self.tool.split(".", 1)[0]

    @property
    def tool_name(self) -> str:
        """The name the LLM sees."""
        return self.tool.split(".", 1)[1]


# ── Fields collected by a "collect" node ──────────────────────────────────────


class NameOptions(_Strict):
    last_name: bool = True
    verify_spelling: bool = False


class DigitsOptions(_Strict):
    num_digits: int = Field(ge=1, le=32, description="How many digits to collect, e.g. 4 for the last 4 digits")
    ask_for_confirmation: bool = False


class BuiltinNameField(_Strict):
    name: Identifier
    builtin: Literal["name"]
    instructions: str = ""
    options: NameOptions = Field(default_factory=NameOptions)


class BuiltinDigitsField(_Strict):
    name: Identifier
    builtin: Literal["digits"]
    instructions: str = ""
    options: DigitsOptions


class BuiltinSimpleField(_Strict):
    name: Identifier
    builtin: Literal["phone", "email", "address", "date_of_birth"]
    instructions: str = ""


class CustomField(_Strict):
    name: Identifier
    type: Literal["string", "number", "integer", "boolean"] = "string"
    description: str = Field(min_length=1, description="What to ask for, in plain words for the LLM")
    enum: list[str] | None = Field(default=None, min_length=1)
    instructions: str = ""


BUILTIN_FIELDS = ("name", "phone", "email", "address", "date_of_birth", "digits")


def _field_discriminator(value: object) -> str:
    """Pick the field model: built-ins with options get their own model, the rest share one.
    An unknown built-in returns its own tag, which pydantic reports as invalid."""
    builtin = value.get("builtin") if isinstance(value, dict) else getattr(value, "builtin", None)
    if builtin is None:
        return "custom"
    if builtin in ("phone", "email", "address", "date_of_birth"):
        return "builtin:simple"
    return f"builtin:{builtin}"


CollectField = Annotated[
    Union[
        Annotated[BuiltinNameField, Tag("builtin:name")],
        Annotated[BuiltinDigitsField, Tag("builtin:digits")],
        Annotated[BuiltinSimpleField, Tag("builtin:simple")],
        Annotated[CustomField, Tag("custom")],
    ],
    Discriminator(_field_discriminator),
]


# ── Nodes ─────────────────────────────────────────────────────────────────────


class AgentNode(_Strict):
    type: Literal["agent"]
    instructions: str = Field(min_length=1)
    transitions: list[Transition] = Field(default_factory=list)
    on_enter: Literal["speak", "wait"] = Field(
        default="speak",
        description="'speak': the agent talks first when this node starts; 'wait': it listens first",
    )
    tools: list[ToolUse] = Field(default_factory=list, description="Tools the LLM may use on this step")


class CollectNode(_Strict):
    type: Literal["collect"]
    instructions: str = Field(default="", description="Extra guidance for the whole collection step")
    fields: list[CollectField] = Field(min_length=1)
    next: Identifier = Field(description="Id of the node to move to once every field is collected")


class ApiNode(_Strict):
    type: Literal["api"]
    tool: str = Field(pattern=TOOL_PATTERN, description="'<integration>.<tool>' to call")
    inputs: dict[str, str] = Field(default_factory=dict, description="The tool's inputs (text with {{variables}})")
    save: list[Identifier] = Field(
        default_factory=list, description="Outputs to keep as variables, e.g. ['minimum_due', 'due_date']"
    )
    say: str = Field(default="", description="Said while the call runs, e.g. 'One moment while I check.'")
    verifies: bool = Field(
        default=False, description="This step checks who the caller is; it must fail when the details don't match"
    )
    next: Identifier = Field(description="Id of the node to move to when the call works")
    on_error: Identifier = Field(description="Id of the node to move to when the call fails")


class EndNode(_Strict):
    type: Literal["end"]
    instructions: str = Field(min_length=1, description="The final words to say before hanging up")
    goodbye: str = Field(
        default="Thank the caller briefly, in the language they are speaking, and say goodbye.",
        description="How to say goodbye just before hanging up",
    )


Node = Annotated[Union[AgentNode, CollectNode, ApiNode, EndNode], Field(discriminator="type")]


class FlowSettings(_Strict):
    language: str = Field(default="hi", pattern=r"^[a-z]{2}(-[A-Za-z]{2})?$|^multi$")
    llm_model: str | None = Field(default=None, description="LiteLLM alias, e.g. 'voice-fast'")
    llm_temperature: float = Field(default=0, ge=0, le=2)
    stt_model: str | None = Field(default=None, description="Deepgram model, e.g. 'nova-3'")
    tts_voice_id: str | None = Field(default=None, description="Cartesia voice id")


class Flow(_Strict):
    schema_version: Literal[1]
    flow_id: Identifier
    name: str = Field(min_length=1)
    persona: str = Field(min_length=1, description="Who the agent is and how it speaks; shared by every node")
    guardrails: list[str] = Field(default_factory=list, description="Rules every node must follow")
    greeting: str | None = Field(
        default=None, description="First thing the caller hears, word for word (can't be interrupted)"
    )
    allow_end_call_anywhere: bool = Field(
        default=True, description="Let the agent hang up from any node if the caller clearly wants to end"
    )
    settings: FlowSettings = Field(default_factory=FlowSettings)
    tools: list[ToolUse] = Field(default_factory=list, description="Tools the LLM may use on every conversation step")
    start_node: Identifier
    nodes: dict[Identifier, Node] = Field(min_length=1)


# ── Validation beyond the shape: the graph and the variables ──────────────────


class FlowError(Exception):
    """Raised by load_flow(); .problems lists every problem found, with where it is."""

    def __init__(self, problems: list[str]) -> None:
        self.problems = problems
        super().__init__("Invalid flow:\n" + "\n".join(f"  - {p}" for p in problems))


AnyNode = Union[AgentNode, CollectNode, ApiNode, EndNode]


def _successors(node: AnyNode) -> list[str]:
    if isinstance(node, AgentNode):
        return [t.to for t in node.transitions]
    if isinstance(node, CollectNode):
        return [node.next]
    if isinstance(node, ApiNode):
        return [node.next, node.on_error]
    return []


def _texts(flow: Flow) -> list[tuple[str, str]]:
    """Every free-text field that may contain {{variables}}, with its location."""
    out = [("persona", flow.persona)]
    out += [(f"guardrails[{i}]", g) for i, g in enumerate(flow.guardrails)]
    if flow.greeting:
        out.append(("greeting", flow.greeting))
    out += _tool_input_texts("tools", flow.tools)
    for nid, node in flow.nodes.items():
        if isinstance(node, ApiNode):
            out += [(f"nodes.{nid}.inputs.{k}", v) for k, v in node.inputs.items()]
            out.append((f"nodes.{nid}.say", node.say))
            continue
        out.append((f"nodes.{nid}.instructions", node.instructions))
        if isinstance(node, AgentNode):
            out += [(f"nodes.{nid}.transitions[{i}].when", t.when) for i, t in enumerate(node.transitions)]
            out += _tool_input_texts(f"nodes.{nid}.tools", node.tools)
        elif isinstance(node, CollectNode):
            out += [(f"nodes.{nid}.fields[{i}].instructions", f.instructions) for i, f in enumerate(node.fields)]
        else:
            out.append((f"nodes.{nid}.goodbye", node.goodbye))
    return out


def _tool_input_texts(loc: str, tools: list[ToolUse]) -> list[tuple[str, str]]:
    return [(f"{loc}[{i}].inputs.{k}", v) for i, t in enumerate(tools) for k, v in t.inputs.items()]


def _variable_places(nid: str, node: AnyNode) -> list[tuple[str, str]]:
    """(variable, where) for every variable a node defines, in order."""
    if isinstance(node, AgentNode):
        return [
            (var, f"nodes.{nid}.transitions[{i}].capture.{var}")
            for i, t in enumerate(node.transitions)
            for var in t.capture
        ]
    if isinstance(node, CollectNode):
        return [(f.name, f"nodes.{nid}.fields[{i}]") for i, f in enumerate(node.fields)]
    if isinstance(node, ApiNode):
        return [(var, f"nodes.{nid}.save[{i}]") for i, var in enumerate(node.save)]
    return []


def defined_variables(flow: Flow) -> dict[str, str]:
    """Every variable a flow defines, mapped to where it's defined."""
    defined: dict[str, str] = {}
    for nid, node in flow.nodes.items():
        for var, where in _variable_places(nid, node):
            defined.setdefault(var, where)
    return defined


def check_flow(flow: Flow) -> list[str]:
    problems: list[str] = []
    ids = set(flow.nodes)

    # Every reference points to a real node.
    if flow.start_node not in ids:
        problems.append(f"start_node: '{flow.start_node}' is not a node (nodes: {', '.join(sorted(ids))})")
    for nid, node in flow.nodes.items():
        if isinstance(node, AgentNode):
            for i, t in enumerate(node.transitions):
                if t.to not in ids:
                    problems.append(f"nodes.{nid}.transitions[{i}].to: '{t.to}' is not a node")
                if t.to == nid:
                    problems.append(f"nodes.{nid}.transitions[{i}].to: a node can't transition to itself")
            names = [t.tool_name for t in node.transitions]
            for dup in sorted({n for n in names if names.count(n) > 1}):
                problems.append(
                    f"nodes.{nid}.transitions: two transitions are both named '{dup}'; give them different 'name's"
                )
            if node.transitions == [] and not flow.allow_end_call_anywhere:
                problems.append(
                    f"nodes.{nid}: has no transitions, and allow_end_call_anywhere is false, so the call gets stuck"
                )
        elif isinstance(node, CollectNode):
            if node.next not in ids:
                problems.append(f"nodes.{nid}.next: '{node.next}' is not a node")
            if node.next == nid:
                problems.append(f"nodes.{nid}.next: a node can't move to itself")
            for i, f in enumerate(node.fields):
                if isinstance(f, CustomField) and f.enum and f.type != "string":
                    problems.append(f"nodes.{nid}.fields[{i}].enum: only string fields can have an enum")
        elif isinstance(node, ApiNode):
            for key in ("next", "on_error"):
                target = getattr(node, key)
                if target not in ids:
                    problems.append(f"nodes.{nid}.{key}: '{target}' is not a node")
                elif target == nid:
                    problems.append(f"nodes.{nid}.{key}: a node can't move to itself")
    problems += _tool_name_problems(flow)
    for nid, node in flow.nodes.items():
        if isinstance(node, AgentNode):
            for i, t in enumerate(node.transitions):
                for var, c in t.capture.items():
                    if c.enum and c.type != "string":
                        problems.append(
                            f"nodes.{nid}.transitions[{i}].capture.{var}.enum: only string values can have an enum"
                        )

    if not any(isinstance(n, EndNode) for n in flow.nodes.values()):
        problems.append("nodes: the flow needs at least one node of type 'end'")

    # Graph checks only make sense once every reference is valid.
    if not problems:
        reachable, queue = {flow.start_node}, deque([flow.start_node])
        while queue:
            for nxt in _successors(flow.nodes[queue.popleft()]):
                if nxt not in reachable:
                    reachable.add(nxt)
                    queue.append(nxt)
        for nid in sorted(ids - reachable):
            problems.append(f"nodes.{nid}: can't be reached from start_node '{flow.start_node}'")

        # Every node must have some path to an end node.
        finishes = {nid for nid, n in flow.nodes.items() if isinstance(n, EndNode)}
        changed = True
        while changed:
            changed = False
            for nid, node in flow.nodes.items():
                if nid not in finishes and any(s in finishes for s in _successors(node)):
                    finishes.add(nid)
                    changed = True
        for nid in sorted(reachable - finishes):
            problems.append(f"nodes.{nid}: no path from here leads to an 'end' node")

    # Variables: each is defined once, doesn't hide a built-in, and every
    # {{reference}} is a built-in or defined somewhere in the flow.
    seen: dict[str, str] = {}
    for nid, node in flow.nodes.items():
        for var, where in _variable_places(nid, node):
            if var in BUILTIN_VARIABLES:
                problems.append(f"{where}: '{var}' is a built-in variable; choose another name")
            elif var in seen:
                problems.append(f"{where}: '{var}' is already defined at {seen[var]}; use a different name")
            else:
                seen[var] = where
    known = set(BUILTIN_VARIABLES) | set(seen)
    for where, text in _texts(flow):
        for var in VARIABLE_RE.findall(text):
            if var not in known:
                problems.append(_unknown_variable(where, var))
    return problems


def _tool_name_problems(flow: Flow) -> list[str]:
    """The LLM sees each tool by its name alone, so names on one step must differ."""
    problems: list[str] = []
    everywhere: set[str] = set()
    for i, t in enumerate(flow.tools):
        if t.tool_name in everywhere:
            problems.append(f"tools[{i}].tool: a tool called '{t.tool_name}' is already in the list")
        everywhere.add(t.tool_name)
    for nid, node in flow.nodes.items():
        if not isinstance(node, AgentNode):
            continue
        transitions = {t.tool_name for t in node.transitions}
        here: set[str] = set()
        for i, t in enumerate(node.tools):
            loc = f"nodes.{nid}.tools[{i}].tool"
            if t.tool_name in everywhere:
                problems.append(f"{loc}: '{t.tool_name}' is already on every step (the flow's tools); remove it here")
            elif t.tool_name in here:
                problems.append(f"{loc}: a tool called '{t.tool_name}' is already on this step")
            elif t.tool_name in transitions:
                problems.append(f"{loc}: a transition on this step is also called '{t.tool_name}'; rename the transition")
            here.add(t.tool_name)
        for k, tr in enumerate(node.transitions):
            if tr.tool_name in everywhere:
                problems.append(
                    f"nodes.{nid}.transitions[{k}]: '{tr.tool_name}' is also the name of a tool on every step; "
                    "rename the transition"
                )
    return problems


def _unknown_variable(where: str, var: str) -> str:
    return (
        f"{where}: unknown variable {{{{{var}}}}}; built-ins are "
        f"{', '.join(sorted(BUILTIN_VARIABLES))}, plus any captured or collected field or saved API output"
    )


def parse_flow(data: dict) -> Flow:
    """Check a flow given as a dict (e.g. from the Go service). Raises FlowError."""
    try:
        flow = Flow.model_validate(data)
    except ValidationError as e:
        problems = [_format_pydantic_error(err) for err in e.errors()]
        # Also report broken links and unknown variables now, so every problem
        # shows up in one round instead of only after the shape errors are fixed.
        problems += [p for p in _best_effort_checks(data) if p not in problems]
        raise FlowError(problems) from None
    problems = check_flow(flow)
    if problems:
        raise FlowError(problems)
    return flow


def load_flow(path: str | Path) -> Flow:
    """Read and check a flow file. Raises FlowError listing every problem."""
    try:
        data = json.loads(Path(path).read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        raise FlowError([f"{path}: not valid JSON (line {e.lineno}, column {e.colno}): {e.msg}"]) from None
    return parse_flow(data)


def _best_effort_checks(data: object) -> list[str]:
    """Link and variable checks on a raw dict that failed shape validation."""
    if not isinstance(data, dict) or not isinstance(data.get("nodes"), dict):
        return []
    nodes: dict = data["nodes"]
    problems: list[str] = []
    start = data.get("start_node")
    if isinstance(start, str) and start not in nodes:
        problems.append(f"start_node: '{start}' is not a node (nodes: {', '.join(sorted(map(str, nodes)))})")

    defined = set(BUILTIN_VARIABLES)
    texts: list[tuple[str, str]] = []
    for key in ("persona", "greeting"):
        if isinstance(data.get(key), str):
            texts.append((key, data[key]))
    for nid, node in nodes.items():
        if not isinstance(node, dict):
            continue
        for key in ("instructions", "goodbye", "say"):
            if isinstance(node.get(key), str):
                texts.append((f"nodes.{nid}.{key}", node[key]))
        transitions = node.get("transitions") or node.get("transitons") or []
        for i, t in enumerate(transitions if isinstance(transitions, list) else []):
            if not isinstance(t, dict):
                continue
            if isinstance(t.get("to"), str) and t["to"] not in nodes:
                problems.append(f"nodes.{nid}.transitions[{i}].to: '{t['to']}' is not a node")
            if isinstance(t.get("capture"), dict):
                defined.update(t["capture"])
            if isinstance(t.get("when"), str):
                texts.append((f"nodes.{nid}.transitions[{i}].when", t["when"]))
        for key in ("next", "on_error"):
            if isinstance(node.get(key), str) and node[key] not in nodes:
                problems.append(f"nodes.{nid}.{key}: '{node[key]}' is not a node")
        save = node.get("save")
        for var in save if isinstance(save, list) else []:
            if isinstance(var, str):
                defined.add(var)
        fields = node.get("fields")
        for i, f in enumerate(fields if isinstance(fields, list) else []):
            if isinstance(f, dict):
                if isinstance(f.get("name"), str):
                    defined.add(f["name"])
                if isinstance(f.get("instructions"), str):
                    texts.append((f"nodes.{nid}.fields[{i}].instructions", f["instructions"]))
    for where, text in texts:
        for var in VARIABLE_RE.findall(text):
            if var not in defined:
                problems.append(_unknown_variable(where, var))
    return problems


def _format_loc(loc: tuple) -> str:
    """('nodes', 'ask', 'agent', 'transitions', 0, 'to') -> 'nodes.ask.transitions[0].to'.

    pydantic puts the chosen union member's tag in the location: right after a
    node id ('agent', 'collect', 'end') and right after a field's index
    ('custom', 'builtin:name', ...). Those are dropped by position, so a node
    that happens to be called "end" keeps its name."""
    out = ""
    for i, part in enumerate(loc):
        if i == 2 and loc[0] == "nodes" and part in ("agent", "collect", "api", "end"):
            continue
        if (
            i >= 2
            and isinstance(loc[i - 1], int)
            and loc[i - 2] == "fields"
            and isinstance(part, str)
            and (part == "custom" or part.startswith("builtin:"))
        ):
            continue
        if isinstance(part, int):
            out += f"[{part}]"
        else:
            out += f".{part}" if out else str(part)
    return out


def _format_pydantic_error(err: dict) -> str:
    loc = _format_loc(err["loc"])
    msg = err["msg"]
    if err["type"] == "extra_forbidden":
        msg = "unknown key (check the spelling)"
    elif err["type"] == "string_pattern_mismatch" and (err.get("ctx") or {}).get("pattern") == IDENTIFIER_PATTERN:
        msg = IDENTIFIER_MESSAGE
    elif err["type"] == "string_pattern_mismatch" and (err.get("ctx") or {}).get("pattern") == TOOL_PATTERN:
        msg = TOOL_MESSAGE
    elif err["type"] == "string_pattern_mismatch":
        msg = "must be a language code like 'hi', 'en', 'hi-IN', or 'multi'"
    elif err["type"] == "union_tag_invalid":
        tag = str((err.get("ctx") or {}).get("tag", ""))
        if tag.startswith("builtin:"):
            loc += ".builtin"
            msg = f"unknown built-in '{tag[len('builtin:'):]}'; use one of {', '.join(BUILTIN_FIELDS)}"
        else:
            msg = f"unknown node type '{tag}'; use 'agent', 'collect', 'api' or 'end'"
    return f"{loc or '(top level)'}: {msg}"


def json_schema() -> dict:
    """JSON Schema of the flow format, for the UI and the Go service."""
    return Flow.model_json_schema()

