"""Flow format (schema version 1).

A *flow* describes one project's conversation as a graph of nodes. It is pure
data: the same flow engine runs every flow, so a new project never needs code
changes. Flow files live in flows/ for now; in Phase 3 they come from the Go
config service, and in Phase 4 the UI edits them.

Node types
----------
agent    A conversation step with its own instructions. It leaves through
         "transitions": the LLM moves the caller on when a transition's "when"
         condition is met, optionally capturing values (e.g. a category).
collect  Collects named fields from the caller, one after another, then moves
         to "next". Fields are either built-in (name, phone, email, address,
         date_of_birth, digits) or custom (described in plain words). The
         caller can go back and correct an earlier answer.
end      Final step: answers any last question, says goodbye and hangs up.

Variables
---------
Text fields can use {{variable}}. Built-ins: caller_number, dialed_number,
now_ist, today_ist. Every captured or collected value is also a variable,
named after the field (e.g. {{issue_summary}}, {{city}}).

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
Identifier = Annotated[str, Field(pattern=r"^[a-z][a-z0-9_]{0,47}$")]
VARIABLE_RE = re.compile(r"\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}")


class _Strict(BaseModel):
    # Unknown keys are errors, so a typo like "transitons" is caught instead of ignored.
    model_config = ConfigDict(extra="forbid")


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


class CollectNode(_Strict):
    type: Literal["collect"]
    instructions: str = Field(default="", description="Extra guidance for the whole collection step")
    fields: list[CollectField] = Field(min_length=1)
    next: Identifier = Field(description="Id of the node to move to once every field is collected")


class EndNode(_Strict):
    type: Literal["end"]
    instructions: str = Field(min_length=1, description="What to say or answer before ending")
    goodbye: str = Field(
        default="Thank the caller briefly, in the language they are speaking, and say goodbye.",
        description="How to say goodbye just before hanging up",
    )


Node = Annotated[Union[AgentNode, CollectNode, EndNode], Field(discriminator="type")]


class FlowSettings(_Strict):
    language: str = Field(default="hi", pattern=r"^[a-z]{2}(-[A-Za-z]{2})?$|^multi$")
    llm_model: str | None = Field(default=None, description="LiteLLM alias, e.g. 'voice-fast'")
    llm_temperature: float = Field(default=0, ge=0, le=2)
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
    start_node: Identifier
    nodes: dict[Identifier, Node] = Field(min_length=1)


# ── Validation beyond the shape: the graph and the variables ──────────────────


class FlowError(Exception):
    """Raised by load_flow(); .problems lists every problem found, with where it is."""

    def __init__(self, problems: list[str]) -> None:
        self.problems = problems
        super().__init__("Invalid flow:\n" + "\n".join(f"  - {p}" for p in problems))


def _successors(node: AgentNode | CollectNode | EndNode) -> list[str]:
    if isinstance(node, AgentNode):
        return [t.to for t in node.transitions]
    if isinstance(node, CollectNode):
        return [node.next]
    return []


def _texts(flow: Flow) -> list[tuple[str, str]]:
    """Every free-text field that may contain {{variables}}, with its location."""
    out = [("persona", flow.persona)]
    out += [(f"guardrails[{i}]", g) for i, g in enumerate(flow.guardrails)]
    if flow.greeting:
        out.append(("greeting", flow.greeting))
    for nid, node in flow.nodes.items():
        out.append((f"nodes.{nid}.instructions", node.instructions))
        if isinstance(node, AgentNode):
            out += [(f"nodes.{nid}.transitions[{i}].when", t.when) for i, t in enumerate(node.transitions)]
        elif isinstance(node, CollectNode):
            out += [(f"nodes.{nid}.fields[{i}].instructions", f.instructions) for i, f in enumerate(node.fields)]
        else:
            out.append((f"nodes.{nid}.goodbye", node.goodbye))
    return out


def defined_variables(flow: Flow) -> dict[str, str]:
    """Every variable a flow defines, mapped to where it's defined."""
    defined: dict[str, str] = {}
    for nid, node in flow.nodes.items():
        if isinstance(node, AgentNode):
            for i, t in enumerate(node.transitions):
                for var in t.capture:
                    defined.setdefault(var, f"nodes.{nid}.transitions[{i}].capture.{var}")
        elif isinstance(node, CollectNode):
            for i, f in enumerate(node.fields):
                defined.setdefault(f.name, f"nodes.{nid}.fields[{i}]")
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
        places: list[tuple[str, str]] = []
        if isinstance(node, AgentNode):
            for i, t in enumerate(node.transitions):
                places += [(var, f"nodes.{nid}.transitions[{i}].capture.{var}") for var in t.capture]
        elif isinstance(node, CollectNode):
            places += [(f.name, f"nodes.{nid}.fields[{i}]") for i, f in enumerate(node.fields)]
        for var, where in places:
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
                problems.append(
                    f"{where}: unknown variable {{{{{var}}}}}; built-ins are "
                    f"{', '.join(sorted(BUILTIN_VARIABLES))}, plus any captured or collected field"
                )
    return problems


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
        for key in ("instructions", "goodbye"):
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
        if isinstance(node.get("next"), str) and node["next"] not in nodes:
            problems.append(f"nodes.{nid}.next: '{node['next']}' is not a node")
        for i, f in enumerate(node.get("fields") or []):
            if isinstance(f, dict):
                if isinstance(f.get("name"), str):
                    defined.add(f["name"])
                if isinstance(f.get("instructions"), str):
                    texts.append((f"nodes.{nid}.fields[{i}].instructions", f["instructions"]))
    for where, text in texts:
        for var in VARIABLE_RE.findall(text):
            if var not in defined:
                problems.append(f"{where}: unknown variable {{{{{var}}}}}")
    return problems


def _format_pydantic_error(err: dict) -> str:
    loc = ".".join(str(p) for p in err["loc"] if not (isinstance(p, str) and p.startswith(("builtin:", "custom"))))
    loc = re.sub(r"\.(\d+)(?=\.|$)", r"[\1]", loc)  # fields.0.name -> fields[0].name
    loc = re.sub(r"\.(agent|collect|end)(?=\.|$)", "", loc)  # drop the union tag pydantic adds
    msg = err["msg"]
    if err["type"] == "extra_forbidden":
        msg = "unknown key (check the spelling)"
    elif err["type"] == "string_pattern_mismatch":
        msg = "must be lower-case letters, digits and _ , starting with a letter (max 48 characters)"
    elif err["type"] == "union_tag_invalid":
        tag = str((err.get("ctx") or {}).get("tag", ""))
        if tag.startswith("builtin:"):
            loc += ".builtin"
            msg = f"unknown built-in '{tag[len('builtin:'):]}'; use one of {', '.join(BUILTIN_FIELDS)}"
        else:
            msg = f"unknown node type '{tag}'; use 'agent', 'collect' or 'end'"
    return f"{loc or '(top level)'}: {msg}"


def json_schema() -> dict:
    """JSON Schema of the flow format, for the UI and the Go service."""
    return Flow.model_json_schema()

