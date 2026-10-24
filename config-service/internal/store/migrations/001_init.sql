-- Flow configuration for the voice platform. No call data is stored here:
-- only flows (prompts, nodes), which number runs which flow, and who changed what.

-- One row per flow (a project's conversation, e.g. emi_payment_issue).
CREATE TABLE flows (
    flow_id    text PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text NOT NULL
);

-- The flow being edited. It may be invalid; problems lists what's wrong.
-- "json" (not jsonb) keeps the text exactly as saved, including key order.
CREATE TABLE flow_drafts (
    flow_id    text PRIMARY KEY REFERENCES flows (flow_id) ON DELETE CASCADE,
    spec       json NOT NULL,
    problems   json NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text NOT NULL
);

-- Published versions: valid, never changed after publishing. Calls run these.
CREATE TABLE flow_versions (
    flow_id      text NOT NULL REFERENCES flows (flow_id) ON DELETE CASCADE,
    version      integer NOT NULL CHECK (version > 0),
    spec         json NOT NULL,
    checksum     text NOT NULL, -- sha256 of spec, hex
    published_at timestamptz NOT NULL DEFAULT now(),
    published_by text NOT NULL,
    PRIMARY KEY (flow_id, version)
);

-- Which flow answers which phone number (E.164, e.g. +918045678901).
-- version_pin NULL = always the latest published version.
CREATE TABLE phone_numbers (
    number      text PRIMARY KEY CHECK (number ~ '^\+[1-9][0-9]{6,14}$'),
    flow_id     text NOT NULL REFERENCES flows (flow_id),
    version_pin integer,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  text NOT NULL,
    FOREIGN KEY (flow_id, version_pin) REFERENCES flow_versions (flow_id, version)
);

-- Who changed which configuration, and when.
CREATE TABLE audit_log (
    id     bigserial PRIMARY KEY,
    at     timestamptz NOT NULL DEFAULT now(),
    actor  text NOT NULL,
    action text NOT NULL,
    target text NOT NULL,
    detail json NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_at ON audit_log (at DESC);
