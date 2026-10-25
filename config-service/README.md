# Config service

Go service that stores every flow (persona, guardrails, nodes, prompts), its
published versions, and which phone number runs which flow, in its own Postgres
(`platform-db`). The voice agent asks it which flow to run at the start of each
call, so publishing a change applies from the next call, with no redeploy.

No call data is stored here: only configuration and an audit log of who changed it.

## How a change goes live

1. **Save a draft**: `PUT /v1/flows/{flow_id}/draft` with the flow JSON. Saved even
   if it has problems (so work isn't lost); the problems are returned.
2. **Publish**: `POST /v1/flows/{flow_id}/publish`. Only a draft with zero problems
   publishes; it becomes version N+1. Versions never change afterwards.
3. **Map a number** (once): `PUT /v1/numbers/{number}` with
   `{"flow_id": "...", "version_pin": null}`. `null` follows the latest version;
   a number pins a version (rollback).

`scripts/publish-flow.sh flows/x.json [+91...]` does all three (`TEAM=emi` for a new flow).

## Who can call it

- **People** sign in to Flow Studio through Keycloak and send its access token.
  Their Keycloak roles and teams decide what they may do:

  | Role | May |
  |---|---|
  | viewer (everyone) | see their teams' flows, drafts and versions |
  | flow-editor | create flows for their teams, save drafts |
  | flow-publisher | publish their teams' flows |
  | platform-admin | everything in every team, phone numbers, audit log, move flows between teams |

  Each flow belongs to one team. Flows of other teams answer 404.
- **Scripts and CI** use `ADMIN_TOKEN` (acts as a platform admin).
- **The voice agent** uses `RUNTIME_TOKEN`, which only works on `/internal/v1/resolve`.

## API

All `/v1` routes need `Authorization: Bearer <token>`. Through Caddy they're at
`https://PUBLIC_HOST/api/v1/...`; `/internal` is never exposed publicly.

| Method and path | Needs | What it does |
|---|---|---|
| `GET /healthz`, `GET /readyz` | none | Up? / Up and database reachable? |
| `GET /v1/me` | signed in | Who you are: email, roles, teams |
| `POST /v1/validate` | signed in | Check a flow without saving it |
| `GET /v1/flows` | viewer | Your teams' flows (admins: all) |
| `GET /v1/flows/{flow_id}` | viewer | One flow's summary |
| `PUT /v1/flows/{flow_id}/draft[?team=x]` | flow-editor | Save the draft; the first save creates the flow in team x |
| `GET /v1/flows/{flow_id}/draft` | viewer | The draft and its problems |
| `POST /v1/flows/{flow_id}/publish` | flow-publisher | Draft becomes the next version (422 with problems if invalid) |
| `GET /v1/flows/{flow_id}/versions[/{n}]` | viewer | Published versions / one version with its flow |
| `PUT /v1/flows/{flow_id}/team` | platform-admin | `{"team": "emi"}`: give the flow to a team |
| `GET /v1/numbers`, `PUT`/`DELETE /v1/numbers/{number}` | platform-admin | Number → flow mapping (`{"flow_id": "...", "version_pin": null}`) |
| `GET /v1/audit?limit=100` | platform-admin | Who changed what |
| `PUT /v1/flows/{flow_id}/layout` | flow-editor | Where Flow Studio draws the nodes (not part of the flow) |
| `POST /v1/flows/{flow_id}/test-sessions` | flow-editor or flow-publisher | "Test call": a LiveKit room token with the agent dispatched to run the saved draft (5 minutes; 20 per person per hour) |
| `GET /internal/v1/resolve?number=...` | RUNTIME_TOKEN | For the agent: the flow a number runs now; ETag / 304 |
| `GET /internal/v1/flows/{flow_id}/draft` | RUNTIME_TOKEN | For the agent in a test call: the saved draft (422 if it has problems) |

Saving a draft with `If-Match: "<checksum>"` (from GET draft) fails with 409 if someone
else saved in between; `If-None-Match: *` creates a new flow only if the id is free.

Numbers are stored in E.164 (`+918045678901`); `91 80 4567 8901` and similar are accepted.

## Validation

`internal/flowspec` is a port of the agent's `flow_engine/spec.py`. Both must
report the same problems: `tests/flow_conformance.json` (generated from the
Python validator by `tests/make_flow_conformance.py`) is checked by both test
suites. When a rule changes, change both, regenerate the file, and run both.

## Run the tests

```bash
cd config-service
go test ./internal/flowspec/                     # no database needed
TEST_DATABASE_URL='postgres://user:pass@localhost:5432/cfgtest?sslmode=disable' go test ./...
```
The API tests wipe the database they're given.
