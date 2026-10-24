"""tests/flow_conformance.json is what both validators (this Python one and the
Go one in config-service/) must report. This test fails when the file is out of
date with the Python rules; regenerate it with:
    python tests/make_flow_conformance.py
The Go side checks the same file in config-service/internal/flowspec."""

import json

from make_flow_conformance import OUT, build, render


def test_conformance_file_is_up_to_date():
    assert OUT.read_text(encoding="utf-8") == render(build()), (
        "tests/flow_conformance.json is out of date: run python tests/make_flow_conformance.py"
    )


def test_conformance_file_has_valid_and_invalid_cases():
    cases = json.loads(OUT.read_text(encoding="utf-8"))
    assert any(not c["problems"] for c in cases)
    assert sum(1 for c in cases if c["problems"]) > 50
