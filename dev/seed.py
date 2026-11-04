"""First-time setup of the local platform (dev/compose.yaml runs it on every
`make dev`). Only adds what's missing, so flows, members and integrations you
changed locally stay as they are.

  Keycloak    both realms (infra/keycloak/setup.py), and demo users whose
              password is DEV_USER_PASSWORD from dev/.env
  workspaces  emi, cards, complaints; Asha maker (and owner), Ravi checker,
              Vera viewer in emi; admin@demo.corp is a platform admin
  integrations  the demo bank (platform) and the EMI workspace's own MCP
              servers emi_core and emi_helpdesk (examples/emi-mcp)
  flows       complaint, credit card and EMI (with tools), published

Only the Python standard library is used.
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
API = os.environ["CONFIG_SERVICE_URL"].rstrip("/")
ADMIN = os.environ["CONFIG_ADMIN_TOKEN"]
PASSWORD = os.environ["DEV_USER_PASSWORD"]

USERS = [
    # email, name, directory teams, directory roles
    ("asha@demo.corp", "Asha Rao", ["emi", "cards", "complaints"], ["flow-editor"]),
    ("ravi@demo.corp", "Ravi Iyer", ["emi", "cards", "complaints"], ["flow-publisher"]),
    ("vera@demo.corp", "Vera Shah", ["emi"], []),
    ("admin@demo.corp", "Platform Admin", [], ["platform-admin"]),
]
WORKSPACES = {"emi": "EMI collections", "cards": "Credit cards", "complaints": "Complaints"}
MEMBERS = {  # workspace -> email -> (maker, checker, owner)
    ws: {
        "asha@demo.corp": (True, False, True),
        "ravi@demo.corp": (False, True, False),
        **({"vera@demo.corp": (False, False, False)} if ws == "emi" else {}),
    }
    for ws in WORKSPACES
}
FLOWS = [  # file, workspace
    ("flows/complaint-helpline.json", "complaints"),
    ("flows/credit-card-helpline.json", "cards"),
    ("examples/emi-mcp/emi-payment-issue.json", "emi"),
]
EMI_SERVERS = [  # id, label, address inside the local network, token variable
    ("emi_core", "EMI loans (core system)", "http://emi-core:8000/mcp", "EMI_CORE_TOKEN"),
    ("emi_helpdesk", "EMI help desk", "http://emi-helpdesk:8000/mcp", "EMI_HELPDESK_TOKEN"),
]


def api(method: str, path: str, body=None, ok=(200, 201, 204)):
    data = body if isinstance(body, bytes) or body is None else json.dumps(body).encode()
    req = urllib.request.Request(API + path, data=data, method=method)
    req.add_header("Authorization", f"Bearer {ADMIN}")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        if e.code in ok:
            return e.code, None
        sys.exit(f"seed: {method} {path} failed ({e.code}): {e.read().decode(errors='replace')[:800]}")


def keycloak() -> None:
    spec = importlib.util.spec_from_file_location("kc_setup", REPO / "infra" / "keycloak" / "setup.py")
    setup = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(setup)
    cfg = setup.settings()
    kc = setup.Keycloak(cfg)
    with contextlib.redirect_stdout(io.StringIO()):  # its own progress lines and one-off passwords
        setup.setup_directory(kc, cfg)
        setup.setup_platform(kc, cfg)
        for email, name, teams, roles in USERS:
            setup.add_user(kc, cfg, email, name, teams, roles, reset=False)
            user = kc.get(f"/{setup.DIRECTORY}/users?username={email}&exact=true")[0]
            kc.call("PUT", f"/{setup.DIRECTORY}/users/{user['id']}/reset-password", {"type": "password", "value": PASSWORD, "temporary": False})
    print("✓ Keycloak: realms and demo users")


def workspaces() -> None:
    for ws, name in WORKSPACES.items():
        api("PUT", f"/v1/workspaces/{ws}", {"name": name})
        _, res = api("GET", f"/v1/workspaces/{ws}/members")
        have = {m["email"] for m in (res or {}).get("members", [])}
        for email, (maker, checker, owner) in MEMBERS[ws].items():
            if email not in have:
                api("PUT", f"/v1/workspaces/{ws}/members/{email}", {"maker": maker, "checker": checker, "owner": owner})
    print("✓ workspaces: " + ", ".join(WORKSPACES))


def integrations() -> None:
    _, res = api("GET", "/v1/integrations")
    have = {i["integration_id"] for i in res["integrations"]}
    added = []
    if "demo_bank" not in have:
        api("PUT", "/v1/integrations/demo_bank", json.loads((REPO / "infra/integrations/demo_bank.json").read_text()))
        added.append("demo_bank")
    for iid, label, url, token_var in EMI_SERVERS:
        if iid in have:
            continue
        tools = json.loads((REPO / f"examples/emi-mcp/{iid}.tools.json").read_text())
        api(
            "PUT",
            f"/v1/integrations/{iid}",
            {
                "kind": "mcp",
                "label": label,
                "notes": "Local demo server (examples/emi-mcp).",
                "url": url,
                "workspace": "emi",
                "auth": {"kind": "header", "header": "Authorization", "secret": f"Bearer {os.environ[token_var]}"},
                "tools": tools,
            },
        )
        added.append(iid)
    print("✓ integrations: demo_bank, emi_core, emi_helpdesk" + (f" (added {', '.join(added)})" if added else ""))


def flows() -> None:
    added = []
    for file, ws in FLOWS:
        spec = (REPO / file).read_bytes()
        flow_id = json.loads(spec)["flow_id"]
        status, _ = api("GET", f"/v1/flows/{flow_id}", ok=(200, 404))
        if status == 200:
            continue
        _, saved = api("PUT", f"/v1/flows/{flow_id}/draft?workspace={ws}", spec)
        if not saved.get("valid"):
            sys.exit(f"seed: {file} has problems: {saved.get('problems')}")
        api("POST", f"/v1/flows/{flow_id}/publish")
        added.append(flow_id)
    print("✓ flows: complaint, credit card, EMI" + (f" (published {', '.join(added)})" if added else ""))


if __name__ == "__main__":
    keycloak()
    workspaces()
    integrations()
    flows()
    print()
    print("Ready: http://localhost:3000")
    print("Sign in as asha@demo.corp (maker), ravi@demo.corp (checker), vera@demo.corp (viewer)")
    print("or admin@demo.corp, with DEV_USER_PASSWORD from dev/.env.")
