# Running the platform on your laptop

Everything runs on your Mac, so you build and test before you push: Flow
Studio, sign-in (Keycloak), the config service, a LiveKit server, the voice
agent, LiteLLM, and the demo systems flows call (demo bank, EMI MCP servers).
Demo users, workspaces, flows and integrations are set up for you. Nothing
here touches EC2, LiveKit Cloud or GitHub.

## Once

- **Docker Desktop** for Mac, with at least **6 GB** of memory for it
  (Settings → Resources).
- `make` and `python3`: if `make --version` fails, run `xcode-select --install`.

## Start

From the repo folder:

```
make dev
```

The first run takes 5 to 10 minutes: it downloads and builds the images,
starts Keycloak for the first time and sets everything up. Later runs take
about a minute. It ends with:

```
Ready: http://localhost:3000
```

Open **http://localhost:3000** (not 127.0.0.1) and sign in. The password for every
demo user is `DEV_USER_PASSWORD` in `dev/.env` (made for you on the first run):

| User | In Flow Studio |
|---|---|
| `asha@demo.corp` | maker (and owner) in the emi, cards and complaints workspaces |
| `ravi@demo.corp` | checker in the same workspaces: approves Asha's changes |
| `vera@demo.corp` | viewer in emi |
| `admin@demo.corp` | platform admin |

Ready to use: the complaint, credit card and EMI flows (published); the demo bank
(a platform integration); and the EMI workspace's own MCP servers `emi_core` and
`emi_helpdesk` (from `examples/emi-mcp`, with sealed tokens).

| Command | |
|---|---|
| `make dev` | start, or apply a change to `dev/.env` or `dev/compose.yaml` |
| `make down` | stop; your local flows, users and changes stay |
| `make reset` | stop and delete all local data; the next `make dev` starts fresh |
| `make logs` / `make logs s=agent` | follow the logs (all services, or one) |
| `make ps` | what's running |
| `make config` | rebuild the config service after changing Go code |
| `make rebuild` | rebuild after changing `requirements.txt`, `package.json` or a Dockerfile |

## Spoken test calls need your own keys

Without any keys you can do everything in Flow Studio: build and change flows,
submit and approve, add integrations and **Connect & load tools** from MCP
servers. A **Test call** needs speech and an LLM: put your own development keys
in `dev/.env`, then run `make dev` again:

```
OPENAI_API_KEY=...     # the LLM (through LiteLLM, as on EC2)
DEEPGRAM_API_KEY=...   # speech-to-text
CARTESIA_API_KEY=...   # the voice
```

Then open a flow, press **Test call**, allow the microphone and talk. The canvas
follows the call. Try the EMI flow with Ravi's loan (last 4 digits **8765**, date of
birth **2 November 1985**) or Asha's (**4321**, **14 May 1990**).

## Changing code

| You change | What happens |
|---|---|
| Flow Studio (`flow-studio/src`) | the page updates by itself |
| The agent (`agent.py`, `flow_engine/`) | the agent restarts by itself within a few seconds; start a new test call (one in progress ends) |
| The config service (Go) | `make config` |
| `flows/*.json` | used only as the agent's fallback flow; to change a flow, edit it in Flow Studio |
| `requirements.txt`, `package.json`, a Dockerfile | `make rebuild` |

Run the tests as before, from the repo folder, then push:

```
python3 -m venv .venv && source .venv/bin/activate && pip install -r requirements-dev.txt   # once
pytest -q
cd config-service && go test ./...      # Go installed locally; the database tests need TEST_DATABASE_URL
cd flow-studio && npm install && npm test
```

## Useful addresses

| | |
|---|---|
| http://localhost:3000 | Flow Studio |
| http://localhost:8180/auth/admin | Keycloak's admin console (`admin` / `KEYCLOAK_ADMIN_PASSWORD`) |
| http://localhost:8080 | the config service API (`Authorization: Bearer <CONFIG_ADMIN_TOKEN>`) |
| http://localhost:4000 | LiteLLM (`LITELLM_MASTER_KEY`) |
| `localhost:5433` | the database: `psql -h localhost -p 5433 -U platform platform` (`PLATFORM_DB_PASSWORD`) |
| http://localhost:8900, :8901, :8902 | demo bank, emi-core, emi-helpdesk (each needs its token from `dev/.env`) |

All of these listen on 127.0.0.1 only, so nobody else on your network can reach them.

**Adding an MCP server locally**: inside the stack, services reach each other by
name, so the EMI servers are `http://emi-core:8000/mcp` and
`http://emi-helpdesk:8000/mcp`. A server you run on your Mac itself (outside
Docker) is `http://host.docker.internal:<port>/mcp`. Your team's server
elsewhere works too if your Mac can reach it (VPN).

## How it differs from EC2

| EC2 | Here |
|---|---|
| HTTPS at your domain, through Caddy | `http://localhost:3000`, through Vite (hot reload) |
| LiveKit Cloud, phone numbers | a LiveKit server on your Mac: browser test calls only, no phone calls |
| Noise cancellation (LiveKit Cloud) | off (`NOISE_CANCELLATION=off`) |
| Users from the company directory | four demo users |
| The agent image, deployed by GitHub Actions | the same image running your files, restarted on changes |

Flows, reviews, integrations, sealed tokens, the agent and its tools are the same code.

## When something's wrong

- **`make dev` stops at "The setup failed"**: the lines above it say why. Fix
  that and run `make dev` again; it picks up where it stopped.
- **Sign-in fails or loops**: use http://localhost:3000 (not 127.0.0.1). `make logs s=keycloak`.
- **Test call says "Waiting for the agent…"**: `make logs s=agent` should end with
  `registered worker`. Right after you save agent code it restarts: wait a few seconds.
- **The agent's log shows `livekit-nc … noise cancellation is not authorized`**: expected
  here (that's a LiveKit Cloud feature); calls work.
- **A port is already in use** (3000, 4000, 5433, 7880–7882, 8080, 8180, 8900–8902): stop
  whatever uses it, or change the left-hand port in `dev/compose.yaml`.
- **Start over**: `make reset && make dev`.
