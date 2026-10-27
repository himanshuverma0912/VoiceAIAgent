"""Builds tests/flow_conformance.json: flow inputs plus the problems the Python
validator reports for them.

The Go config service has its own validator (so the UI gets errors when a flow is
saved). Both validators must give the same answers; the Python test checks this
file is up to date, and the Go tests check Go reports exactly these problems.

Regenerate after changing validation rules:
    python tests/make_flow_conformance.py
"""

from __future__ import annotations

import copy
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from flow_engine.spec import FlowError, parse_flow  # noqa: E402

OUT = ROOT / "tests" / "flow_conformance.json"

BASE = {
    "schema_version": 1,
    "flow_id": "demo",
    "name": "Demo",
    "persona": "You help callers.",
    "guardrails": ["Be polite."],
    "greeting": "Hello",
    "allow_end_call_anywhere": True,
    "settings": {"language": "hi", "llm_model": "voice-fast", "llm_temperature": 0, "tts_voice_id": None},
    "start_node": "ask",
    "nodes": {
        "ask": {
            "type": "agent",
            "on_enter": "wait",
            "instructions": "Ask what they need.",
            "transitions": [
                {
                    "to": "details",
                    "when": "They said what they need.",
                    "capture": {
                        "topic": {"type": "string", "description": "Topic", "enum": ["a", "b"]},
                        "count": {"type": "integer", "description": "How many", "required": False},
                    },
                }
            ],
        },
        "details": {
            "type": "collect",
            "instructions": "Collect details.",
            "fields": [
                {"name": "full_name", "builtin": "name", "options": {"last_name": True, "verify_spelling": False}},
                {"name": "phone", "builtin": "phone", "instructions": "Caller is {{caller_number}}."},
                {"name": "pin", "builtin": "digits", "options": {"num_digits": 4, "ask_for_confirmation": False}},
                {"name": "city", "type": "string", "description": "City", "enum": ["x", "y"]},
            ],
            "next": "done",
        },
        "done": {
            "type": "end",
            "instructions": "Thank {{full_name}} about {{topic}} at {{now_ist}}.",
            "goodbye": "Bye.",
        },
    },
}


def case(name: str, change) -> dict:
    flow = copy.deepcopy(BASE)
    change(flow)
    return {"name": name, "flow": flow}


def set_path(path: list, value):
    def change(flow):
        target = flow
        for key in path[:-1]:
            target = target[key]
        target[path[-1]] = value

    return change


def delete_path(path: list):
    def change(flow):
        target = flow
        for key in path[:-1]:
            target = target[key]
        del target[path[-1]]

    return change


def rename_key(path: list, old: str, new: str):
    def change(flow):
        target = flow
        for key in path:
            target = target[key]
        target[new] = target.pop(old)

    return change


def many(*changes):
    def change(flow):
        for c in changes:
            c(flow)

    return change


N = ["nodes"]
ASK_T = ["nodes", "ask", "transitions", 0]
FIELDS = ["nodes", "details", "fields"]

CASES = [
    case("valid base flow", lambda f: None),
    # ── shape: keys, required fields, types ──
    case("unknown top-level key", set_path(["colour"], "blue")),
    case("typo in node key", rename_key(["nodes", "ask"], "transitions", "transitons")),
    case("missing persona", delete_path(["persona"])),
    case("missing nodes", delete_path(["nodes"])),
    case("schema_version 2", set_path(["schema_version"], 2)),
    case("empty name", set_path(["name"], "")),
    case("bad flow_id", set_path(["flow_id"], "Demo Flow")),
    case("bad node id", many(rename_key(["nodes"], "ask", "Ask Me"), set_path(["start_node"], "Ask Me"))),
    case("bad language", set_path(["settings", "language"], "hindi")),
    case("temperature too high", set_path(["settings", "llm_temperature"], 3)),
    case("unknown node type", set_path(["nodes", "done", "type"], "finish")),
    case("missing node type", delete_path(["nodes", "done", "type"])),
    case("bad on_enter", set_path(["nodes", "ask", "on_enter"], "talk")),
    case("transition missing when", delete_path(ASK_T + ["when"])),
    case("capture bad type", set_path(ASK_T + ["capture", "topic", "type"], "text")),
    case("unknown builtin", set_path(FIELDS + [0, "builtin"], "full_name")),
    case("digits missing num_digits", set_path(FIELDS + [2, "options"], {})),
    case("digits num_digits too big", set_path(FIELDS + [2, "options", "num_digits"], 40)),
    case("custom field missing description", delete_path(FIELDS + [3, "description"])),
    case("empty fields", set_path(FIELDS, [])),
    case("end missing instructions", delete_path(["nodes", "done", "instructions"])),
    case("guardrail not a string", set_path(["guardrails"], [1])),
    case("empty object", lambda f: f.clear()),
    case("nodes is a list", set_path(["nodes"], [])),
    case("nodes is empty", set_path(["nodes"], {})),
    case("node is a string", set_path(["nodes", "done"], "bye")),
    case("settings is a string", set_path(["settings"], "fast")),
    case("transitions is an object", set_path(["nodes", "ask", "transitions"], {})),
    case("transition is a string", set_path(["nodes", "ask", "transitions"], ["details"])),
    case("capture is a list", set_path(ASK_T + ["capture"], [])),
    case("capture value is a string", set_path(ASK_T + ["capture", "topic"], "Topic")),
    case("bad capture key", set_path(ASK_T + ["capture", "Bad Key"], {"description": "d"})),
    case("field is a string", set_path(FIELDS + [0], "full_name")),
    case("fields is a number", set_path(FIELDS, 5)),
    case("name options is a string", set_path(FIELDS + [0, "options"], "x")),
    case("builtin is a number", set_path(FIELDS + [0, "builtin"], 5)),
    case("builtin is null", set_path(FIELDS + [3, "builtin"], None)),
    case("node type is a number", set_path(["nodes", "done", "type"], 5)),
    case("num_digits as text", set_path(FIELDS + [2, "options", "num_digits"], "4")),
    case("num_digits with decimals", set_path(FIELDS + [2, "options", "num_digits"], 4.5)),
    case("num_digits zero", set_path(FIELDS + [2, "options", "num_digits"], 0)),
    case("required as text", set_path(ASK_T + ["capture", "count", "required"], "yes")),
    case("allow_end_call_anywhere as text", set_path(["allow_end_call_anywhere"], "true")),
    case("temperature as text", set_path(["settings", "llm_temperature"], "0.5")),
    case("temperature negative", set_path(["settings", "llm_temperature"], -1)),
    case("temperature decimal", set_path(["settings", "llm_temperature"], 0.7)),
    case("schema_version as text", set_path(["schema_version"], "1")),
    case("flow_id a number", set_path(["flow_id"], 5)),
    case("name null", set_path(["name"], None)),
    case("greeting null", set_path(["greeting"], None)),
    case("greeting a number", set_path(["greeting"], 5)),
    case("guardrails null", set_path(["guardrails"], None)),
    case("enum not a list", set_path(ASK_T + ["capture", "topic", "enum"], "a")),
    case("enum empty", set_path(ASK_T + ["capture", "topic", "enum"], [])),
    case("enum item a number", set_path(ASK_T + ["capture", "topic", "enum"], [1])),
    case("transition name bad", set_path(ASK_T + ["name"], "Go Next")),
    case("transition name null", set_path(ASK_T + ["name"], None)),
    case("language with region", set_path(["settings", "language"], "hi-IN")),
    case("language multi", set_path(["settings", "language"], "multi")),
    case("stt model set", set_path(["settings", "stt_model"], "nova-2")),
    case("stt model null", set_path(["settings", "stt_model"], None)),
    case("stt model a number", set_path(["settings", "stt_model"], 3)),
    case("collect missing next", delete_path(["nodes", "details", "next"])),
    case("unknown key in options", set_path(FIELDS + [2, "options", "digits"], 4)),
    case(
        "nodes named like node types",
        many(
            rename_key(["nodes"], "done", "end"),
            set_path(["nodes", "details", "next"], "end"),
            set_path(["nodes", "end", "goodbye"], 5),
        ),
    ),
    case(
        "node named customer_care",
        many(
            rename_key(["nodes"], "details", "customer_care"),
            set_path(ASK_T + ["to"], "customer_care"),
            set_path(["nodes", "customer_care", "fields", 3, "description"], ""),
        ),
    ),
    # ── links and graph ──
    case("transition to missing node", set_path(ASK_T + ["to"], "detials")),
    case("next to missing node", set_path(["nodes", "details", "next"], "dnoe")),
    case("start node missing", set_path(["start_node"], "welcome")),
    case("transition to itself", set_path(ASK_T + ["to"], "ask")),
    case(
        "duplicate transition names",
        lambda f: f["nodes"]["ask"]["transitions"].append({"to": "details", "when": "again"}),
    ),
    case("no end node", set_path(["nodes", "done"], {"type": "agent", "instructions": "Chat."})),
    case("unreachable node", set_path(["nodes", "orphan"], {"type": "end", "instructions": "x"})),
    case(
        "loop with no way out",
        many(
            lambda f: f["nodes"]["ask"]["transitions"].append({"to": "loop_a", "when": "loop"}),
            set_path(["nodes", "loop_a"], {"type": "agent", "instructions": "A", "transitions": [{"to": "loop_b", "when": "x"}]}),
            set_path(["nodes", "loop_b"], {"type": "agent", "instructions": "B", "transitions": [{"to": "loop_a", "when": "x"}]}),
        ),
    ),
    case(
        "dead end when ending anywhere is off",
        many(
            set_path(["allow_end_call_anywhere"], False),
            lambda f: f["nodes"]["ask"]["transitions"].append({"to": "stuck", "when": "x"}),
            set_path(["nodes", "stuck"], {"type": "agent", "instructions": "S"}),
        ),
    ),
    case("enum on a number capture", set_path(ASK_T + ["capture", "count", "enum"], ["1"])),
    case("enum on a number field", set_path(FIELDS + [3], {"name": "city", "type": "number", "description": "n", "enum": ["1"]})),
    # ── variables ──
    case("unknown variable", set_path(["nodes", "done", "goodbye"], "Bye {{custmer}}.")),
    case("variable defined twice", set_path(FIELDS + [3, "name"], "topic")),
    case("variable shadows builtin", set_path(FIELDS + [1, "name"], "caller_number")),
    # ── shape errors and link errors together ──
    case(
        "shape plus link plus variable",
        many(
            set_path(["nodes", "ask", "instrucions"], "typo"),
            set_path(ASK_T + ["to"], "dnoe"),
            set_path(["nodes", "done", "goodbye"], "Bye {{who}}."),
        ),
    ),
]


# ── tools and API steps ──

API_BASE = copy.deepcopy(BASE)
API_BASE["tools"] = [{"tool": "bank.search_faq"}]
API_BASE["nodes"]["ask"]["tools"] = [
    {"tool": "bank.recent_transactions", "inputs": {"card_last4": "{{pin}}"}},
]
API_BASE["nodes"]["details"]["next"] = "check"
API_BASE["nodes"]["check"] = {
    "type": "api",
    "tool": "bank.get_card_balance",
    "inputs": {"card_last4": "{{pin}}", "date_of_birth": "{{full_name}}"},
    "save": ["minimum_due", "due_date"],
    "say": "One moment, {{full_name}}.",
    "verifies": False,
    "next": "done",
    "on_error": "done",
}
API_BASE["nodes"]["done"]["instructions"] = "Minimum due {{minimum_due}} by {{due_date}}."
CHECK = ["nodes", "check"]


def api_case(name: str, change) -> dict:
    flow = copy.deepcopy(API_BASE)
    change(flow)
    return {"name": name, "flow": flow}


CASES += [
    api_case("valid flow with tools and an api step", lambda f: None),
    api_case("api step without on_error", delete_path(CHECK + ["on_error"])),
    api_case("api step tool without integration", set_path(CHECK + ["tool"], "get_card_balance")),
    api_case("api step tool with capitals and dash", set_path(CHECK + ["tool"], "bank.Get-Balance")),
    api_case("api step next is not a node", set_path(CHECK + ["next"], "dnoe")),
    api_case("api step on_error to itself", set_path(CHECK + ["on_error"], "check")),
    api_case("api step with instructions", set_path(CHECK + ["instructions"], "Do it.")),
    api_case("api step bad save name", set_path(CHECK + ["save"], ["Minimum Due"])),
    api_case("api step input is a number", set_path(CHECK + ["inputs", "count"], 5)),
    api_case("api step say unknown variable", set_path(CHECK + ["say"], "Hold on {{who}}.")),
    api_case("api step verifies not a boolean", set_path(CHECK + ["verifies"], "yes")),
    api_case("api step without tool", delete_path(CHECK + ["tool"])),
    api_case("flow tools item is a string", set_path(["tools"], ["bank.search_faq"])),
    api_case("flow tools listed twice", set_path(["tools"], [{"tool": "bank.search_faq"}, {"tool": "help.search_faq"}])),
    api_case("step tool also on every step", set_path(["nodes", "ask", "tools"], [{"tool": "bank.search_faq"}])),
    api_case(
        "step tool listed twice",
        set_path(["nodes", "ask", "tools"], [{"tool": "bank.find_branch"}, {"tool": "other.find_branch"}]),
    ),
    api_case("step tool named like a transition", set_path(["nodes", "ask", "tools"], [{"tool": "bank.go_to_details"}])),
    api_case("transition named like a flow tool", set_path(ASK_T + ["name"], "search_faq")),
    api_case("step tool unknown key", set_path(["nodes", "ask", "tools", 0, "locked"], True)),
    api_case("tool input unknown variable", set_path(["tools", 0, "inputs"], {"q": "{{question_text}}"})),
    api_case("save collides with a collected field", set_path(CHECK + ["save"], ["city"])),
    api_case("save shadows a built-in", set_path(CHECK + ["save"], ["today_ist"])),
    api_case("tools on a collect step", set_path(["nodes", "details", "tools"], [{"tool": "bank.search_faq"}])),
    api_case(
        "api step loops back so the end is unreachable",
        many(set_path(CHECK + ["next"], "ask"), set_path(CHECK + ["on_error"], "ask")),
    ),
    api_case(
        "api shape plus link plus variable",
        many(
            set_path(CHECK + ["sya"], "typo"),
            set_path(CHECK + ["on_error"], "nowhere"),
            set_path(CHECK + ["say"], "Wait {{nobody}}."),
        ),
    ),
]

CASES.append({"name": "top level is a list", "flow": []})


def build() -> list[dict]:
    out = []
    for c in CASES:
        try:
            parse_flow(copy.deepcopy(c["flow"]))
            problems: list[str] = []
        except FlowError as e:
            problems = e.problems
        out.append({**c, "problems": problems})
    return out


def render(cases: list[dict]) -> str:
    return json.dumps(cases, ensure_ascii=False, indent=2) + "\n"


if __name__ == "__main__":
    OUT.write_text(render(build()), encoding="utf-8")
    print(f"wrote {len(CASES)} cases to {OUT.relative_to(ROOT)}")
