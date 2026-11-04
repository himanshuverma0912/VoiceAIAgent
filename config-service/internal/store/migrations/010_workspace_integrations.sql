-- Self-service integrations: makers add MCP servers / REST APIs to their own
-- workspace; a checker of that workspace approves before live calls use them.
--
--   owner_workspace  NULL: added by a platform admin (offered as "workspaces"
--                    says); otherwise the workspace that owns it (only it uses it)
--   live             false until a checker first approves it
--   auth             how the agent signs in: {"kind": "header" | "oauth", ...},
--                    with the secret sealed for the agent (internal/secrets):
--                    nothing here can read it
--   pending          a proposed change (same shape as the live definition)
--                    waiting for a checker; drafts and test calls already use it
ALTER TABLE integrations ADD COLUMN owner_workspace text REFERENCES workspaces (workspace_id) ON DELETE RESTRICT;
ALTER TABLE integrations ADD COLUMN live boolean NOT NULL DEFAULT true;
ALTER TABLE integrations ADD COLUMN auth jsonb NOT NULL DEFAULT '{}';
ALTER TABLE integrations ADD COLUMN pending jsonb;
ALTER TABLE integrations ADD COLUMN pending_by text NOT NULL DEFAULT '';
ALTER TABLE integrations ADD COLUMN pending_at timestamptz;
ALTER TABLE integrations ADD COLUMN pending_note text NOT NULL DEFAULT '';
ALTER TABLE integrations ADD COLUMN approved_by text NOT NULL DEFAULT '';
ALTER TABLE integrations ADD COLUMN approved_at timestamptz;
ALTER TABLE integrations ADD COLUMN rejected_by text NOT NULL DEFAULT '';
ALTER TABLE integrations ADD COLUMN rejected_at timestamptz;
ALTER TABLE integrations ADD COLUMN rejected_note text NOT NULL DEFAULT '';
CREATE INDEX integrations_owner ON integrations (owner_workspace);
