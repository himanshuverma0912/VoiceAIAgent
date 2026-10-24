"""Check flow files from the command line.

    python -m flow_engine flows/complaint-helpline.json [more files...]

Exit code 0 when every file is valid, 1 otherwise (so CI can use it).
"""

import sys

from .spec import FlowError, load_flow


def main(paths: list[str]) -> int:
    if not paths:
        print("usage: python -m flow_engine <flow.json> [more.json ...]", file=sys.stderr)
        return 2
    failed = 0
    for path in paths:
        try:
            flow = load_flow(path)
        except FileNotFoundError:
            print(f"✗ {path}: file not found")
            failed += 1
            continue
        except FlowError as e:
            print(f"✗ {path} has {len(e.problems)} problem(s):")
            for problem in e.problems:
                print(f"  - {problem}")
            failed += 1
            continue
        print(f"✓ {path}: flow '{flow.flow_id}' is valid ({len(flow.nodes)} nodes, starts at '{flow.start_node}')")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
