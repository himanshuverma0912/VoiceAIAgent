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

`scripts/publish-flow.sh flows/x.json [+91...]` does all three.

## API

Tokens: `Authorization: Bearer <token>`. `ADMIN_TOKEN` for `/v1/*`,
`RUNTIME_TOKEN` (the agent's) for `/internal/v1/*`. The agent's token can't
change anything.

| Method and path | What it does |
|---|---|
| `GET /healthz` | Up? (no token) |
| `GET /readyz` | Up and database reachable? (no token) |
| `POST /v1/validate` | Check a flow without saving it |
| `GET /v1/flows` | All flows: latest version, draft valid?, numbers |
| `GET /v1/flows/{flow_id}` | One flow's summary |
| `PUT /v1/flows/{flow_id}/draft` | Save the draft (creates the flow the first time) |
| `GET /v1/flows/{flow_id}/draft` | The draft and its problems |
| `POST /v1/flows/{flow_id}/publish` | Draft becomes the next version (422 with problems if invalid) |
| `GET /v1/flows/{flow_id}/versions` | Published versions |
| `GET /v1/flows/{flow_id}/versions/{n}` | One version, with its flow |
| `GET /v1/numbers` | Number → flow mapping |
| `PUT /v1/numbers/{number}` | Map a number: `{"flow_id": "...", "version_pin": null}` |
| `DELETE /v1/numbers/{number}` | Unmap (calls then use the agent's FLOW_FILE) |
| `GET /v1/audit?limit=100` | Who changed what |
| `GET /internal/v1/resolve?number=...` | For the agent: the flow a number runs now; ETag / 304 |

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
