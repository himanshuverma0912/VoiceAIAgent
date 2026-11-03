-- Drop-resume: where a caller who hung up mid-flow can pick up again, for 15
-- minutes and once. The agent seals it (AES-GCM, key only in the agent's .env)
-- and files it under an HMAC of the caller's number: this table never holds a
-- phone number or anything readable the caller said.
CREATE TABLE resume_points (
    key          text PRIMARY KEY,           -- HMAC of the caller's number (64 hex)
    flow_id      text NOT NULL,
    flow_version integer,
    sealed       text NOT NULL,              -- base64 of nonce + ciphertext
    expires_at   timestamptz NOT NULL
);
CREATE INDEX resume_points_expires ON resume_points (expires_at);

-- Call history: whether a call picked up from a dropped one, and whether it
-- left a point to pick up from. Never where, or what was kept.
ALTER TABLE calls ADD COLUMN resumed boolean NOT NULL DEFAULT false;
ALTER TABLE calls ADD COLUMN resume_saved boolean NOT NULL DEFAULT false;
