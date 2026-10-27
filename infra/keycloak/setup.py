#!/usr/bin/env python3
"""Sets up Keycloak for Flow Studio sign-in. Safe to run again: it creates what's
missing and updates what's there, so run it after any change to this file.

Two realms:

  corp-idp        Stands in for the company directory (Azure AD / Entra ID in the
                  enterprise). Users and their groups live here. It signs people
                  in and tells voice-platform who they are over SAML, exactly as
                  Entra ID will.
  voice-platform  The platform's own realm. It trusts corp-idp over SAML and gives
                  Flow Studio and the config service OIDC tokens (easier for a
                  browser app and Go than SAML). Directory groups become:
                    team-<name>          -> Keycloak group /teams/<name>
                    role-flow-editor     -> realm role flow-editor
                    role-flow-publisher  -> realm role flow-publisher
                    role-platform-admin  -> realm role platform-admin  (everything)
                  Everyone gets "viewer".

Workspaces: what people may do in Flow Studio is decided by their workspace
memberships (maker, checker, owner), managed in Flow Studio, not here. Teams and
roles from this file are used once, at a person's first sign-in, to give them
their first memberships: each team becomes a workspace they're in, flow-editor
makes them a maker there and flow-publisher a checker. platform-admin is the only
role that keeps meaning afterwards.

Swapping corp-idp for Entra ID later changes only the identity provider entry in
voice-platform; Flow Studio and the config service stay the same.

Usage (on EC2, from ~/infra; reads PUBLIC_HOST and KEYCLOAK_ADMIN_PASSWORD from .env):
  python3 keycloak/setup.py setup
  python3 keycloak/setup.py add-user asha@demo.corp --name "Asha Rao" --teams emi --roles flow-editor,flow-publisher
  python3 keycloak/setup.py add-user ravi@demo.corp --name "Ravi Iyer" --teams complaints --roles flow-editor
  python3 keycloak/setup.py list-users

Only the Python standard library is used (Ubuntu has python3).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import secrets
import string
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

PLATFORM = "voice-platform"
DIRECTORY = "corp-idp"
IDP_ALIAS = "corp-idp"
STUDIO_CLIENT = "flow-studio"
API_AUDIENCE = "config-api"
ROLES = {
    "viewer": "Signed in to Flow Studio",
    "flow-editor": "First sign-in: maker in their teams' workspaces",
    "flow-publisher": "First sign-in: checker in their teams' workspaces",
    "platform-admin": "All workspaces, phone numbers, model catalog and the audit log",
}
GRANTABLE_ROLES = ["flow-editor", "flow-publisher", "platform-admin"]  # viewer is automatic
TEAM_RE = re.compile(r"^[a-z][a-z0-9_-]{0,31}$")
EMAIL_NAMEID = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"


# ── Settings ──────────────────────────────────────────────────────────────────


def load_env_file(path: Path) -> None:
    """Load KEY=VALUE lines into os.environ (existing variables win)."""
    if not path.exists():
        return
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        os.environ.setdefault(key.strip(), value.strip().strip('"').strip("'"))


def settings() -> dict:
    load_env_file(Path(__file__).resolve().parent.parent / ".env")
    host = os.environ.get("PUBLIC_HOST", "").strip()
    password = os.environ.get("KEYCLOAK_ADMIN_PASSWORD", "").strip()
    if not host or not password:
        sys.exit("PUBLIC_HOST and KEYCLOAK_ADMIN_PASSWORD must be set (in ~/infra/.env)")
    public = os.environ.get("PUBLIC_URL", f"https://{host}").rstrip("/")
    return {
        "public": public,  # what browsers use, e.g. https://voiceflow.duckdns.org
        "kc_public": f"{public}/auth",
        # Admin API: Keycloak's port on this machine (not exposed to the internet).
        "admin_url": os.environ.get("KEYCLOAK_ADMIN_URL", "http://127.0.0.1:8180/auth").rstrip("/"),
        "admin_user": os.environ.get("KEYCLOAK_ADMIN_USER", "admin"),
        "admin_password": password,
    }


# ── Admin REST API ────────────────────────────────────────────────────────────


class Keycloak:
    def __init__(self, cfg: dict) -> None:
        self.base = cfg["admin_url"]
        form = urllib.parse.urlencode(
            {
                "grant_type": "password",
                "client_id": "admin-cli",
                "username": cfg["admin_user"],
                "password": cfg["admin_password"],
            }
        ).encode()
        req = urllib.request.Request(f"{self.base}/realms/master/protocol/openid-connect/token", data=form)
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                self.token = json.load(resp)["access_token"]
        except urllib.error.HTTPError as e:
            sys.exit(f"Keycloak admin sign-in failed ({e.code}); check KEYCLOAK_ADMIN_PASSWORD")
        except urllib.error.URLError as e:
            sys.exit(f"Keycloak not reachable at {self.base} ({e.reason}); is the container up?")

    def call(self, method: str, path: str, body=None, ok_missing: bool = False):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(f"{self.base}/admin/realms{path}", data=data, method=method)
        req.add_header("Authorization", f"Bearer {self.token}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                raw = resp.read()
                location = resp.headers.get("Location", "")
        except urllib.error.HTTPError as e:
            if ok_missing and e.code == 404:
                return None
            detail = e.read().decode(errors="replace")[:500]
            sys.exit(f"Keycloak {method} {path} failed: {e.code} {detail}")
        if raw:
            return json.loads(raw)
        return location.rsplit("/", 1)[-1] if location else None

    def get(self, path: str):
        return self.call("GET", path, ok_missing=True)


def upsert(kc: Keycloak, list_path: str, items: list, match: str, rep: dict, item_path: str) -> str:
    """Create rep under list_path, or update the existing item whose `match` field equals rep's."""
    existing = next((i for i in items if i.get(match) == rep[match]), None)
    if existing is None:
        return kc.call("POST", list_path, rep)
    kc.call("PUT", f"{item_path}/{existing['id']}", {**existing, **rep})
    return existing["id"]


# ── Realms ────────────────────────────────────────────────────────────────────


def ensure_realm(kc: Keycloak, realm: str, display: str) -> None:
    rep = {
        "realm": realm,
        "enabled": True,
        "displayName": display,
        "registrationAllowed": False,
        "resetPasswordAllowed": False,
        "loginWithEmailAllowed": True,
        "duplicateEmailsAllowed": False,
        "bruteForceProtected": True,
        "sslRequired": "external",
        "accessTokenLifespan": 300,
        "ssoSessionIdleTimeout": 1800,
        "ssoSessionMaxLifespan": 36000,
    }
    if realm == DIRECTORY:
        rep["passwordPolicy"] = "length(12) and notUsername"
    if kc.get(f"/{realm}") is None:
        kc.call("POST", "", rep)
        print(f"  created realm {realm}")
    else:
        kc.call("PUT", f"/{realm}", rep)
        print(f"  realm {realm} up to date")


def ensure_group(kc: Keycloak, realm: str, path: str) -> str:
    """Group by path, e.g. "/teams/emi" or "team-emi"; creates parents as needed."""
    parts = [p for p in path.strip("/").split("/") if p]
    parent_id = None
    for depth in range(len(parts)):
        sub_path = "/" + "/".join(parts[: depth + 1])
        found = kc.get(f"/{realm}/group-by-path/{urllib.parse.quote(sub_path.lstrip('/'))}")
        if found:
            parent_id = found["id"]
            continue
        if parent_id is None:
            parent_id = kc.call("POST", f"/{realm}/groups", {"name": parts[depth]})
        else:
            parent_id = kc.call("POST", f"/{realm}/groups/{parent_id}/children", {"name": parts[depth]})
    return parent_id


def mapper_upsert(kc: Keycloak, list_path: str, rep: dict) -> None:
    upsert(kc, list_path, kc.get(list_path) or [], "name", rep, list_path)


# ── corp-idp: the stand-in company directory ──────────────────────────────────


def saml_property_mapper(attr: str) -> dict:
    return {
        "name": attr,
        "protocol": "saml",
        "protocolMapper": "saml-user-property-mapper",
        "config": {"user.attribute": attr, "attribute.name": attr, "attribute.nameformat": "Basic", "friendly.name": attr},
    }


def setup_directory(kc: Keycloak, cfg: dict) -> None:
    ensure_realm(kc, DIRECTORY, "Demo Corp directory (stands in for Azure AD)")
    for g in [f"role-{r}" for r in GRANTABLE_ROLES]:
        ensure_group(kc, DIRECTORY, g)

    # The SAML "application" for voice-platform, like an Enterprise Application in Entra ID.
    broker = f"{cfg['kc_public']}/realms/{PLATFORM}/broker/{IDP_ALIAS}/endpoint"
    sp_entity_id = f"{cfg['kc_public']}/realms/{PLATFORM}"
    rep = {
        "clientId": sp_entity_id,
        "name": "Voice Flow Platform",
        "protocol": "saml",
        "enabled": True,
        "redirectUris": [broker, f"{broker}/*"],
        "frontchannelLogout": True,
        "attributes": {
            "saml.assertion.signature": "true",
            "saml.server.signature": "true",
            "saml.client.signature": "false",
            "saml.encrypt": "false",
            "saml.authnstatement": "true",
            "saml_signature_algorithm": "RSA_SHA256",
            "saml.force.name.id.format": "true",
            "saml_name_id_format": "email",
            "saml_assertion_consumer_url_post": broker,
            "saml_single_logout_service_url_post": broker,
        },
    }
    clients = kc.get(f"/{DIRECTORY}/clients?clientId={urllib.parse.quote(sp_entity_id)}") or []
    client_id = upsert(kc, f"/{DIRECTORY}/clients", clients, "clientId", rep, f"/{DIRECTORY}/clients")
    mappers = f"/{DIRECTORY}/clients/{client_id}/protocol-mappers/models"
    for attr in ("email", "firstName", "lastName"):
        mapper_upsert(kc, mappers, saml_property_mapper(attr))
    mapper_upsert(
        kc,
        mappers,
        {
            "name": "groups",
            "protocol": "saml",
            "protocolMapper": "saml-group-membership-mapper",
            "config": {"attribute.name": "groups", "attribute.nameformat": "Basic", "full.path": "false", "single": "true"},
        },
    )
    print(f"  SAML application for {PLATFORM} in {DIRECTORY} up to date")


def directory_signing_certificate(kc: Keycloak) -> str:
    keys = kc.get(f"/{DIRECTORY}/keys") or {}
    for key in keys.get("keys", []):
        if key.get("algorithm") == "RS256" and key.get("status", "ACTIVE") == "ACTIVE" and key.get("certificate"):
            return key["certificate"]
    sys.exit(f"no active RS256 signing key in realm {DIRECTORY}")


# ── voice-platform: the platform realm ────────────────────────────────────────


def setup_platform(kc: Keycloak, cfg: dict) -> None:
    ensure_realm(kc, PLATFORM, "Voice Flow Platform")
    for name, description in ROLES.items():
        if kc.get(f"/{PLATFORM}/roles/{name}") is None:
            kc.call("POST", f"/{PLATFORM}/roles", {"name": name, "description": description})
        else:
            kc.call("PUT", f"/{PLATFORM}/roles/{name}", {"name": name, "description": description})
    viewer = kc.get(f"/{PLATFORM}/roles/viewer")
    defaults = kc.get(f"/{PLATFORM}/roles/default-roles-{PLATFORM}/composites") or []
    if not any(r["name"] == "viewer" for r in defaults):
        kc.call("POST", f"/{PLATFORM}/roles/default-roles-{PLATFORM}/composites", [viewer])
    ensure_group(kc, PLATFORM, "/teams")
    print(f"  roles {', '.join(ROLES)} up to date (everyone gets viewer)")

    # Audience-only client: tokens for Flow Studio say they're meant for the config API.
    api = {
        "clientId": API_AUDIENCE,
        "name": "Config service API",
        "protocol": "openid-connect",
        "enabled": True,
        "publicClient": False,
        "bearerOnly": True,
        "standardFlowEnabled": False,
        "directAccessGrantsEnabled": False,
        "serviceAccountsEnabled": False,
    }
    upsert(kc, f"/{PLATFORM}/clients", kc.get(f"/{PLATFORM}/clients?clientId={API_AUDIENCE}") or [], "clientId", api, f"/{PLATFORM}/clients")

    # Flow Studio: a browser app, so a public client using Authorization Code + PKCE.
    studio = {
        "clientId": STUDIO_CLIENT,
        "name": "Flow Studio",
        "protocol": "openid-connect",
        "enabled": True,
        "publicClient": True,
        "standardFlowEnabled": True,
        "implicitFlowEnabled": False,
        "directAccessGrantsEnabled": False,
        "serviceAccountsEnabled": False,
        "frontchannelLogout": True,
        "rootUrl": cfg["public"],
        "baseUrl": "/",
        "redirectUris": [f"{cfg['public']}/*"],
        "webOrigins": [cfg["public"]],
        "attributes": {
            "pkce.code.challenge.method": "S256",
            "post.logout.redirect.uris": f"{cfg['public']}/*",
        },
    }
    studio_id = upsert(
        kc, f"/{PLATFORM}/clients", kc.get(f"/{PLATFORM}/clients?clientId={STUDIO_CLIENT}") or [], "clientId", studio, f"/{PLATFORM}/clients"
    )
    mappers = f"/{PLATFORM}/clients/{studio_id}/protocol-mappers/models"
    mapper_upsert(
        kc,
        mappers,
        {
            "name": "audience config-api",
            "protocol": "openid-connect",
            "protocolMapper": "oidc-audience-mapper",
            "config": {"included.custom.audience": API_AUDIENCE, "access.token.claim": "true", "id.token.claim": "false"},
        },
    )
    mapper_upsert(
        kc,
        mappers,
        {
            "name": "groups",
            "protocol": "openid-connect",
            "protocolMapper": "oidc-group-membership-mapper",
            "config": {
                "claim.name": "groups",
                "full.path": "true",
                "access.token.claim": "true",
                "id.token.claim": "true",
                "userinfo.token.claim": "true",
            },
        },
    )
    print(f"  clients {STUDIO_CLIENT} (PKCE) and {API_AUDIENCE} up to date")

    # Trust corp-idp over SAML, like Keycloak will trust Entra ID.
    sso = f"{cfg['kc_public']}/realms/{DIRECTORY}/protocol/saml"
    idp = {
        "alias": IDP_ALIAS,
        "displayName": "Company sign-in",
        "providerId": "saml",
        "enabled": True,
        "trustEmail": True,
        "storeToken": False,
        "firstBrokerLoginFlowAlias": "first broker login",
        "config": {
            "entityId": f"{cfg['kc_public']}/realms/{PLATFORM}",
            "idpEntityId": f"{cfg['kc_public']}/realms/{DIRECTORY}",
            "singleSignOnServiceUrl": sso,
            "singleLogoutServiceUrl": sso,
            "nameIDPolicyFormat": EMAIL_NAMEID,
            "principalType": "SUBJECT",
            "postBindingResponse": "true",
            "postBindingAuthnRequest": "true",
            "postBindingLogout": "true",
            "validateSignature": "true",
            "signingCertificate": directory_signing_certificate(kc),
            "wantAssertionsSigned": "true",
            "wantAuthnRequestsSigned": "false",
            "signatureAlgorithm": "RSA_SHA256",
            # FORCE: name, email, teams and roles are refreshed from the directory at every sign-in.
            "syncMode": "FORCE",
        },
    }
    if kc.get(f"/{PLATFORM}/identity-provider/instances/{IDP_ALIAS}") is None:
        kc.call("POST", f"/{PLATFORM}/identity-provider/instances", idp)
    else:
        kc.call("PUT", f"/{PLATFORM}/identity-provider/instances/{IDP_ALIAS}", idp)
    mappers = f"/{PLATFORM}/identity-provider/instances/{IDP_ALIAS}/mappers"
    for attr in ("email", "firstName", "lastName"):
        mapper_upsert(
            kc,
            mappers,
            {
                "name": attr,
                "identityProviderAlias": IDP_ALIAS,
                "identityProviderMapper": "saml-user-attribute-idp-mapper",
                "config": {"attribute.name": attr, "user.attribute": attr, "syncMode": "INHERIT"},
            },
        )
    for role in GRANTABLE_ROLES:
        mapper_upsert(
            kc,
            mappers,
            {
                "name": f"role {role}",
                "identityProviderAlias": IDP_ALIAS,
                "identityProviderMapper": "saml-role-idp-mapper",
                "config": {"attribute.name": "groups", "attribute.value": f"role-{role}", "role": role, "syncMode": "INHERIT"},
            },
        )
    for team in existing_teams(kc):
        ensure_team_mapping(kc, team)
    print(f"  SAML identity provider {IDP_ALIAS} up to date")


def existing_teams(kc: Keycloak) -> list[str]:
    groups = kc.get(f"/{DIRECTORY}/groups?briefRepresentation=true&max=1000") or []
    return sorted(g["name"][len("team-") :] for g in groups if g["name"].startswith("team-"))


def ensure_team_mapping(kc: Keycloak, team: str) -> None:
    """Directory group team-<team> -> platform group /teams/<team>."""
    ensure_group(kc, DIRECTORY, f"team-{team}")
    ensure_group(kc, PLATFORM, f"/teams/{team}")
    mapper_upsert(
        kc,
        f"/{PLATFORM}/identity-provider/instances/{IDP_ALIAS}/mappers",
        {
            "name": f"team {team}",
            "identityProviderAlias": IDP_ALIAS,
            "identityProviderMapper": "saml-advanced-group-idp-mapper",
            "config": {
                "attributes": json.dumps([{"key": "groups", "value": f"team-{team}"}]),
                "are.attribute.values.regex": "false",
                "group": f"/teams/{team}",
                "syncMode": "INHERIT",
            },
        },
    )


# ── Directory users ───────────────────────────────────────────────────────────


def generate_password() -> str:
    alphabet = string.ascii_letters + string.digits
    return "-".join("".join(secrets.choice(alphabet) for _ in range(5)) for _ in range(4))


def add_user(kc: Keycloak, cfg: dict, email: str, name: str, teams: list[str], roles: list[str], reset: bool) -> None:
    for team in teams:
        if not TEAM_RE.match(team):
            sys.exit(f"team '{team}': use lower-case letters, digits, - and _ (start with a letter)")
    for role in roles:
        if role not in GRANTABLE_ROLES:
            sys.exit(f"role '{role}': use {', '.join(GRANTABLE_ROLES)} (everyone is a viewer)")
    for team in teams:
        ensure_team_mapping(kc, team)

    first, _, last = name.partition(" ")
    rep = {
        "username": email,
        "email": email,
        "firstName": first or email,
        "lastName": last or "-",
        "emailVerified": True,
        "enabled": True,
    }
    found = kc.get(f"/{DIRECTORY}/users?username={urllib.parse.quote(email)}&exact=true") or []
    password = None
    if found:
        user_id = found[0]["id"]
        kc.call("PUT", f"/{DIRECTORY}/users/{user_id}", {**found[0], **rep})
        print(f"updated {email}")
    else:
        user_id = kc.call("POST", f"/{DIRECTORY}/users", rep)
        print(f"created {email}")
        reset = True
    if reset:
        password = generate_password()
        kc.call("PUT", f"/{DIRECTORY}/users/{user_id}/reset-password", {"type": "password", "value": password, "temporary": False})

    # Directory groups: exactly the teams and roles given.
    wanted = {f"team-{t}" for t in teams} | {f"role-{r}" for r in roles}
    current = {g["name"]: g["id"] for g in kc.get(f"/{DIRECTORY}/users/{user_id}/groups") or []}
    for gname, gid in current.items():
        if gname.startswith(("team-", "role-")) and gname not in wanted:
            kc.call("DELETE", f"/{DIRECTORY}/users/{user_id}/groups/{gid}")
    for gname in wanted - set(current):
        kc.call("PUT", f"/{DIRECTORY}/users/{user_id}/groups/{ensure_group(kc, DIRECTORY, gname)}")

    print(f"  teams: {', '.join(teams) or '(none)'}; roles: viewer{''.join(', ' + r for r in roles)}")
    if password:
        print(f"  password: {password}")
        print("  (shown once; run add-user again with --reset-password for a new one)")
    print(f"  sign in at {cfg['public']}/ (changes apply at the user's next sign-in)")


def list_users(kc: Keycloak) -> None:
    for user in kc.get(f"/{DIRECTORY}/users?max=1000") or []:
        groups = [g["name"] for g in kc.get(f"/{DIRECTORY}/users/{user['id']}/groups") or []]
        teams = [g[5:] for g in groups if g.startswith("team-")]
        roles = [g[5:] for g in groups if g.startswith("role-")]
        print(f"{user['username']:32} teams: {','.join(teams) or '-':20} roles: {','.join(roles) or 'viewer'}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("setup", help="create or update both realms")
    add = sub.add_parser("add-user", help="create or update a user in the stand-in directory")
    add.add_argument("email")
    add.add_argument("--name", default="", help='e.g. "Asha Rao"')
    add.add_argument("--teams", default="", help="comma-separated, e.g. emi,complaints")
    add.add_argument("--roles", default="", help="comma-separated: flow-editor, flow-publisher, platform-admin")
    add.add_argument("--reset-password", action="store_true")
    sub.add_parser("list-users", help="users in the stand-in directory")
    args = parser.parse_args()

    cfg = settings()
    kc = Keycloak(cfg)
    if args.command == "setup":
        print(f"Setting up Keycloak for {cfg['public']}")
        setup_directory(kc, cfg)
        setup_platform(kc, cfg)
        print("Done. Next: add users with  python3 keycloak/setup.py add-user ...")
    elif args.command == "add-user":
        split = lambda s: [x.strip() for x in s.split(",") if x.strip()]  # noqa: E731
        add_user(kc, cfg, args.email.strip().lower(), args.name.strip(), split(args.teams), split(args.roles), args.reset_password)
    else:
        list_users(kc)


if __name__ == "__main__":
    main()
