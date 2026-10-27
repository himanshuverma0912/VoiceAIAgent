-- Integrations: the systems flows may call during a call (a bank's REST API or
-- an MCP server), managed by platform admins. A flow names a tool as
-- "<integration_id>.<tool>"; a draft that uses anything not listed here, not
-- switched on or not offered to its workspace has a problem.
--
-- No secrets are stored: auth_env names an environment variable of the voice
-- agent (INTEGRATION_...) that holds the header value, e.g. "Bearer ...".
-- tools holds each tool's description, inputs, outputs and rules:
--   sensitive  only usable after a step that verifies the caller
--   locked     inputs the flow must set itself (the AI can't choose them)
--   max_calls  how many times one call may use it
CREATE TABLE integrations (
    integration_id text PRIMARY KEY CHECK (integration_id ~ '^[a-z][a-z0-9_]{0,31}$'),
    kind        text NOT NULL CHECK (kind IN ('mcp', 'http')),
    label       text NOT NULL,
    notes       text NOT NULL DEFAULT '',
    url         text NOT NULL,                 -- MCP endpoint, or the REST API's base URL
    test_url    text NOT NULL DEFAULT '',      -- used by Flow Studio test calls; empty: url
    auth_header text NOT NULL DEFAULT '',      -- e.g. Authorization
    auth_env    text NOT NULL DEFAULT '',      -- e.g. INTEGRATION_DEMO_BANK_TOKEN
    timeout_ms  integer NOT NULL DEFAULT 5000,
    workspaces  text[] NOT NULL DEFAULT '{}',  -- empty: every workspace
    enabled     boolean NOT NULL DEFAULT true,
    tools       json NOT NULL DEFAULT '[]',    -- json, not jsonb: keeps the order
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  text NOT NULL
);

-- Call history: which tools a call used, whether each worked and how long it
-- took. Never the inputs or the answers.
ALTER TABLE calls ADD COLUMN tools jsonb NOT NULL DEFAULT '[]';
