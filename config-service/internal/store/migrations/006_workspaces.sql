-- Workspaces: a workspace holds a group of flows (bots) and the people who
-- work on them. Membership is managed in Flow Studio, not in Keycloak.
--   everyone in it  sees its flows, call history and versions
--   maker           edits drafts and submits them for review
--   checker         approves (publishes) or rejects other people's submissions
--   owner           adds and removes people, sets their roles
-- Platform admins (a Keycloak role) can do all of this in every workspace.
CREATE TABLE workspaces (
    workspace_id text PRIMARY KEY CHECK (workspace_id ~ '^[a-z][a-z0-9_-]{0,31}$'),
    name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   text NOT NULL
);

CREATE TABLE workspace_members (
    workspace_id text NOT NULL REFERENCES workspaces (workspace_id) ON DELETE CASCADE,
    email        text NOT NULL CHECK (email = lower(email) AND email LIKE '%_@_%'),
    maker        boolean NOT NULL DEFAULT false,
    checker      boolean NOT NULL DEFAULT false,
    owner        boolean NOT NULL DEFAULT false,
    added_at     timestamptz NOT NULL DEFAULT now(),
    added_by     text NOT NULL,
    PRIMARY KEY (workspace_id, email)
);
CREATE INDEX workspace_members_by_email ON workspace_members (email);

-- Each team that owns flows becomes a workspace of the same id.
INSERT INTO workspaces (workspace_id, name, created_by)
SELECT DISTINCT team, initcap(replace(replace(team, '_', ' '), '-', ' ')), 'migration'
FROM flows WHERE team IS NOT NULL;

ALTER TABLE flows RENAME COLUMN team TO workspace;
ALTER TABLE flows ADD CONSTRAINT flows_workspace_fk FOREIGN KEY (workspace) REFERENCES workspaces (workspace_id);

ALTER TABLE model_catalog RENAME COLUMN teams TO workspaces;

-- People whose first memberships were copied from their Keycloak groups
-- (/teams/<name> + flow-editor/flow-publisher), so it happens only once:
-- after that, Flow Studio is where membership is changed.
CREATE TABLE directory_imports (
    email       text PRIMARY KEY,
    imported_at timestamptz NOT NULL DEFAULT now()
);
