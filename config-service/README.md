# Config service

Go service that stores every flow (persona, guardrails, nodes, prompts), its
published versions, and which phone number runs which flow, in its own Postgres
(`platform-db`). The voice agent asks it which flow to run at the start of each
call, so publishing a change applies from the next call, with no redeploy.

Besides configuration and an audit log of who changed it, it keeps a
content-free **call history**: for each call, which flow version ran, the steps
it went through and how long each took, how it ended, reply speed and usage
totals. Never the caller's number, the room name, anything said, or any value
the caller gave (records with any other field are refused). Kept for
`CALL_RETENTION_DAYS` (default 90), then deleted.

## How a change goes live

1. **Save a draft**: `PUT /v1/flows/{flow_id}/draft` with the flow JSON. Saved even
   if it has problems (so work isn't lost); the problems are returned.
2. **Submit it for review** (a maker): `POST /v1/flows/{flow_id}/reviews`
   `{"note": "what changed"}`. Only a draft with zero problems, different from
   the live version, can be submitted. The submitted flow is kept as it was, so
   later edits to the draft don't change what's under review. Submitting again
   replaces an open submission ("superseded").
3. **Approve it** (a checker who isn't the maker):
   `POST /v1/flows/{flow_id}/reviews/{id}/approve`. Approving publishes exactly the
   submitted flow as version N+1. Versions never change afterwards. Or reject
   it with a reason (`.../reject`, `{"note"}` required); the maker can withdraw it.
4. **Map a number** (once): `PUT /v1/numbers/{number}` with
   `{"flow_id": "...", "version_pin": null}`. `null` follows the latest version;
   a number pins a version (rollback).

People can't publish directly. Scripts with `ADMIN_TOKEN` can
(`POST /v1/flows/{flow_id}/publish`): `scripts/publish-flow.sh flows/x.json [+91...]`
saves, publishes and maps a number (`WORKSPACE=emi` for a new flow; the
workspace is created if it doesn't exist).

## Who can call it

- **People** sign in to Flow Studio through Keycloak and send its access token.
  What they may do depends on their **workspaces**. Each flow belongs to one
  workspace; people see only the workspaces they're in (other flows answer 404).
  In each workspace a person has any of these roles:

  | Role | May |
  |---|---|
  | member (everyone in it) | see its flows, drafts, versions, reviews and call history |
  | maker | create flows, save drafts, submit them for review, test-call drafts, build flows with AI |
  | checker | approve (publishes) or reject other people's submissions, test-call drafts |
  | owner | add and remove people, set their roles, rename the workspace |

  Nobody approves their own submission, even with both maker and checker.
  The Keycloak role **platform-admin** may do everything in every workspace, and
  creates workspaces, maps phone numbers, reads the audit log and runs the model
  catalog. Owners and admins manage membership in Flow Studio (Workspaces); the
  person is identified by the email they sign in with, and doesn't need to have
  signed in before.

  **First sign-in.** The first time someone signs in, their Keycloak team groups
  (`/teams/<x>`) and roles become their first memberships: `flow-editor` →
  maker, `flow-publisher` → checker, in each of their teams. That happens once
  per person (recorded in `directory_imports`); after that, Flow Studio is
  where membership is managed and Keycloak teams are ignored.
- **Scripts and CI** use `ADMIN_TOKEN` (acts as a platform admin).
- **The voice agent** uses `RUNTIME_TOKEN`, which only works on `/internal/v1/...`.

## API

All `/v1` routes need `Authorization: Bearer <token>`. Through Caddy they're at
`https://PUBLIC_HOST/api/v1/...`; `/internal` is never exposed publicly.

| Method and path | Needs | What it does |
|---|---|---|
| `GET /healthz`, `GET /readyz` | none | Up? / Up and database reachable? |
| `GET /v1/me` | signed in | Who you are: email, roles, workspaces and your roles in each |
| `POST /v1/validate[?workspace=x]` | signed in | Check a flow without saving it (with `workspace`, also against the model catalog as it applies there) |
| `GET /v1/workspaces` | signed in | Your workspaces (admins: all), with flow and member counts |
| `PUT /v1/workspaces/{workspace}` | platform-admin to create; owner to rename | `{"name": "EMI collections"}` |
| `DELETE /v1/workspaces/{workspace}` | platform-admin | Remove an empty workspace (409 while it has flows) |
| `GET /v1/workspaces/{workspace}/members` | member | Who's in it and their roles |
| `PUT`/`DELETE /v1/workspaces/{workspace}/members/{email}` | owner | Add someone or set their roles (`{"maker", "checker", "owner"}`: booleans) / remove them. The last owner can't be removed or lose the role |
| `GET /v1/flows` | signed in | The flows of your workspaces (admins: all), with any review waiting |
| `GET /v1/flows/{flow_id}` | member | One flow's summary |
| `PUT /v1/flows/{flow_id}/draft[?workspace=x]` | maker | Save the draft; the first save creates the flow in workspace x |
| `GET /v1/flows/{flow_id}/draft` | member | The draft and its problems |
| `POST /v1/flows/{flow_id}/reviews` | maker | Submit the saved draft for review (`{"note"}`) |
| `GET /v1/flows/{flow_id}/reviews[/{id}]` | member | The flow's reviews, newest first / one, with the submitted flow |
| `POST /v1/flows/{flow_id}/reviews/{id}/approve` | checker, not the maker | Publish the submitted flow as the next version (`{"note"}` optional) |
| `POST /v1/flows/{flow_id}/reviews/{id}/reject` | checker, not the maker | Close it without publishing (`{"note"}` required) |
| `POST /v1/flows/{flow_id}/reviews/{id}/withdraw` | the maker who submitted it | Take it back |
| `GET /v1/reviews` | signed in | Submissions waiting in your workspaces; `for_me` marks the ones you may decide |
| `POST /v1/flows/{flow_id}/publish` | ADMIN_TOKEN only | Draft becomes the next version directly (scripts, CI) |
| `GET /v1/flows/{flow_id}/versions[/{n}]` | member | Published versions (who submitted, who approved) / one version with its flow |
| `PUT /v1/flows/{flow_id}/workspace` | platform-admin | `{"workspace": "emi"}`: move the flow to another workspace |
| `POST /v1/builds` | maker | AI flow builder: `{"workspace", "flow_id", "name", "description"}` (40 to 400,000 characters). Answers 202 with a `build_id`; the build runs in the background |
| `GET /v1/builds/{build_id}` | whoever started it | `running` (with progress), `done` (the flow, notes for the business team, any problems left) or `failed`. Kept for an hour, in memory only |
| `GET /v1/numbers`, `PUT`/`DELETE /v1/numbers/{number}` | platform-admin | Number → flow mapping (`{"flow_id": "...", "version_pin": null}`) |
| `GET /v1/audit?limit=100` | platform-admin | Who changed what |
| `PUT /v1/flows/{flow_id}/layout` | maker | Where Flow Studio draws the nodes (not part of the flow) |
| `POST /v1/flows/{flow_id}/test-sessions` | maker or checker | "Test call": a LiveKit room token with the agent dispatched to run the saved draft (5 minutes; 20 per person per hour) |
| `GET /v1/flows/{flow_id}/calls?days=7&channel=phone&limit=50` | member | Recent calls (channel `phone`, `studio_test` or `all`) |
| `GET /v1/flows/{flow_id}/calls/summary?days=7&channel=phone` | member | Outcomes, durations, reply speed, where callers drop off, per version and per day |
| `GET /v1/catalog` | signed in | The models flows may use (LLMs, speech-to-text models, voices); admins also see switched-off ones |
| `PUT /v1/catalog/{kind}/{model_id}` | platform-admin | Add or change one: kind `llm` (LiteLLM alias), `stt` (Deepgram model) or `tts_voice` (Cartesia voice id); `{"label", "notes", "languages": [], "workspaces": [], "enabled": true}` |
| `DELETE /v1/catalog/{kind}/{model_id}` | platform-admin | Remove one nothing uses (409 lists the flows that still do) |
| `GET /internal/v1/resolve?number=...` | RUNTIME_TOKEN | For the agent: the flow a number runs now; ETag / 304 |
| `GET /internal/v1/flows/{flow_id}/draft` | RUNTIME_TOKEN | For the agent in a test call: the saved draft (422 if it has problems) |
| `POST /internal/v1/calls` | RUNTIME_TOKEN | For the agent when a call ends: its call-history record (sending it again is a no-op) |

`?team=` still works wherever `?workspace=` does, for older scripts.

Saving a draft with `If-Match: "<checksum>"` (from GET draft) fails with 409 if someone
else saved in between; `If-None-Match: *` creates a new flow only if the id is free.

Numbers are stored in E.164 (`+918045678901`); `91 80 4567 8901` and similar are accepted.

## AI flow builder

Business teams describe a call in plain words (a long brief is fine: 14–20k
tokens is well within the limit) and get a flow with all its steps and
transitions. The service sends the description, with a guide to the flow
format and an example flow (`internal/builder/`), to the LLM behind the
LiteLLM alias `FLOW_BUILDER_MODEL` (default `flow-builder`, a stronger model
than the voice agent's), checks the answer with the same validator as
drafts, and sends any problems back for the model to fix (up to 3 attempts).
The result is only a draft: a maker reviews it in the editor, test-calls it and
submits it like any other change. The description isn't stored: builds live in
memory for an hour (at most one running per person, three at once). Set
`LITELLM_URL` (and `LITELLM_API_KEY`) to switch it on; without them
`POST /v1/builds` answers 503.

## Model catalog

A flow's `settings.llm_model`, `settings.stt_model` and `settings.tts_voice_id`
must be in the catalog, switched on, offered to the flow's workspace and (if the
entry lists languages) support the flow's language. Otherwise the draft has a
problem and can't be test-called or published. Empty means the agent's default.
Switching an entry off doesn't stop published versions that use it; their next
publish needs another model. A new LLM must exist as an alias in
`infra/litellm-config.yaml` before it's added here.

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
