"""Tests for the flow format and validator (flow_engine/spec.py).

Run:  pip install -r requirements-dev.txt && pytest -q
"""

import copy
import json
from pathlib import Path

import pytest

from flow_engine.spec import FlowError, json_schema, load_flow, parse_flow

ROOT = Path(__file__).resolve().parent.parent
FLOW_FILES = sorted((ROOT / "flows").glob("*.json"))

# Smallest valid flow; each test breaks one thing.
MINIMAL = {
    "schema_version": 1,
    "flow_id": "demo",
    "name": "Demo",
    "persona": "You are a helpful assistant.",
    "start_node": "ask",
    "nodes": {
        "ask": {
            "type": "agent",
            "instructions": "Ask how you can help.",
            "transitions": [{"to": "done", "when": "The caller has said what they need."}],
        },
        "done": {"type": "end", "instructions": "Answer any last question."},
    },
}


def problems_for(change) -> list[str]:
    data = copy.deepcopy(MINIMAL)
    change(data)
    with pytest.raises(FlowError) as exc:
        parse_flow(data)
    return exc.value.problems


def test_minimal_flow_is_valid():
    flow = parse_flow(copy.deepcopy(MINIMAL))
    assert flow.start_node == "ask"
    assert flow.nodes["ask"].transitions[0].tool_name == "go_to_done"


@pytest.mark.parametrize("path", FLOW_FILES, ids=lambda p: p.name)
def test_every_flow_file_in_repo_is_valid(path):
    load_flow(path)


def test_transition_to_missing_node():
    p = problems_for(lambda d: d["nodes"]["ask"]["transitions"][0].update(to="dnoe"))
    assert p == ["nodes.ask.transitions[0].to: 'dnoe' is not a node"]


def test_start_node_missing():
    p = problems_for(lambda d: d.update(start_node="welcome"))
    assert p[0].startswith("start_node: 'welcome' is not a node")


def test_needs_an_end_node():
    def change(d):
        d["nodes"]["done"] = {"type": "agent", "instructions": "Chat."}

    assert "nodes: the flow needs at least one node of type 'end'" in problems_for(change)


def test_unreachable_node():
    def change(d):
        d["nodes"]["orphan"] = {"type": "end", "instructions": "Never used."}

    assert problems_for(change) == ["nodes.orphan: can't be reached from start_node 'ask'"]


def test_node_with_no_way_to_finish():
    def change(d):
        d["nodes"]["ask"]["transitions"].append({"to": "loop_a", "when": "Caller wants a loop."})
        d["nodes"]["loop_a"] = {"type": "agent", "instructions": "A", "transitions": [{"to": "loop_b", "when": "x"}]}
        d["nodes"]["loop_b"] = {"type": "agent", "instructions": "B", "transitions": [{"to": "loop_a", "when": "x"}]}

    assert problems_for(change) == [
        "nodes.loop_a: no path from here leads to an 'end' node",
        "nodes.loop_b: no path from here leads to an 'end' node",
    ]


def test_transition_to_itself():
    p = problems_for(lambda d: d["nodes"]["ask"]["transitions"].append({"to": "ask", "when": "again"}))
    assert "nodes.ask.transitions[1].to: a node can't transition to itself" in p


def test_duplicate_transition_names():
    p = problems_for(lambda d: d["nodes"]["ask"]["transitions"].append({"to": "done", "when": "also done"}))
    assert any("both named 'go_to_done'" in x for x in p)


def test_typo_in_key_is_caught():
    def change(d):
        d["nodes"]["ask"]["transitons"] = d["nodes"]["ask"].pop("transitions")

    assert "nodes.ask.transitons: unknown key (check the spelling)" in problems_for(change)


def test_unknown_node_type():
    p = problems_for(lambda d: d["nodes"]["done"].update(type="finish"))
    assert any(x.startswith("nodes.done") and "unknown node type" in x for x in p)


def test_bad_identifier():
    def change(d):
        d["nodes"]["Ask Me"] = d["nodes"].pop("ask")
        d["start_node"] = "Ask Me"

    assert any("must be lower-case letters" in x for x in problems_for(change))


def test_unknown_variable():
    p = problems_for(lambda d: d["nodes"]["done"].update(instructions="Thank {{custmer_name}}."))
    assert p == [
        "nodes.done.instructions: unknown variable {{custmer_name}}; built-ins are caller_number, "
        "dialed_number, now_ist, today_ist, plus any captured or collected field"
    ]


def test_builtin_and_captured_variables_are_known():
    def change(d):
        d["nodes"]["ask"]["transitions"][0]["capture"] = {"need": {"description": "What they need"}}
        d["nodes"]["done"]["instructions"] = "Recap {{need}} for {{caller_number}} at {{now_ist}}."

    data = copy.deepcopy(MINIMAL)
    change(data)
    parse_flow(data)


def test_variable_defined_twice():
    def change(d):
        d["nodes"]["ask"]["transitions"][0]["capture"] = {"city": {"description": "City"}}
        d["nodes"]["ask"]["transitions"][0]["to"] = "details"
        d["nodes"]["details"] = {"type": "collect", "fields": [{"name": "city", "description": "City"}], "next": "done"}

    assert any("'city' is already defined at nodes.ask.transitions[0].capture.city" in x for x in problems_for(change))


def test_variable_shadowing_builtin():
    def change(d):
        d["nodes"]["ask"]["transitions"][0]["capture"] = {"caller_number": {"description": "x"}}

    assert any("'caller_number' is a built-in variable" in x for x in problems_for(change))


def test_digits_field_needs_num_digits():
    def change(d):
        d["nodes"]["ask"]["transitions"][0]["to"] = "verify"
        d["nodes"]["verify"] = {
            "type": "collect",
            "fields": [{"name": "last4", "builtin": "digits", "options": {}}],
            "next": "done",
        }

    assert any("verify.fields[0].options.num_digits" in x and "required" in x.lower() for x in problems_for(change))


def test_enum_only_on_strings():
    def change(d):
        d["nodes"]["ask"]["transitions"][0]["capture"] = {
            "count": {"type": "integer", "description": "How many", "enum": ["1", "2"]}
        }

    assert any("only string values can have an enum" in x for x in problems_for(change))


def test_all_problems_reported_at_once():
    def change(d):
        d["start_node"] = "nope"
        d["nodes"]["ask"]["transitions"][0]["to"] = "missing"

    assert len(problems_for(change)) == 2


def test_invalid_json_file(tmp_path):
    bad = tmp_path / "bad.json"
    bad.write_text('{"schema_version": 1,,}')
    with pytest.raises(FlowError) as exc:
        load_flow(bad)
    assert "not valid JSON (line 1" in exc.value.problems[0]


def test_json_schema_is_exported():
    schema = json_schema()
    assert schema["title"] == "Flow"
    assert "nodes" in schema["properties"]
    json.dumps(schema)  # serialisable for the UI / Go service


def test_unknown_builtin_names_the_valid_options():
    def change(d):
        d["nodes"]["ask"]["transitions"][0]["to"] = "details"
        d["nodes"]["details"] = {"type": "collect", "fields": [{"name": "n", "builtin": "full_name"}], "next": "done"}

    assert problems_for(change) == [
        "nodes.details.fields[0].builtin: unknown built-in 'full_name'; "
        "use one of name, phone, email, address, date_of_birth, digits"
    ]


def test_shape_errors_and_link_errors_reported_together():
    def change(d):
        d["nodes"]["ask"]["instrucions"] = "typo"  # shape error
        d["nodes"]["ask"]["transitions"][0]["to"] = "dnoe"  # broken link
        d["nodes"]["done"]["instructions"] = "Bye {{who}}."  # unknown variable

    assert problems_for(change) == [
        "nodes.ask.instrucions: unknown key (check the spelling)",
        "nodes.ask.transitions[0].to: 'dnoe' is not a node",
        "nodes.done.instructions: unknown variable {{who}}",
    ]
