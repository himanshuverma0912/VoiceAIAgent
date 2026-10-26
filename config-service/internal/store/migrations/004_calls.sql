-- Call history: content-free facts about each call, sent by the agent when the
-- call ends. Never the caller's number, the room name, anything said, or any
-- value the caller gave; the API refuses fields that aren't listed here.
-- Rows older than CALL_RETENTION_DAYS (default 90) are deleted.
CREATE TABLE calls (
    call_id        text PRIMARY KEY,          -- LiveKit job id
    flow_id        text NOT NULL,
    flow_version   integer,                   -- null for drafts and the fallback file
    flow_source    text NOT NULL,
    channel        text NOT NULL,             -- phone | studio_test
    runtime        text NOT NULL,             -- livekit (pipecat later)
    started_at     timestamptz NOT NULL,
    duration_ms    integer NOT NULL,
    outcome        text NOT NULL,
    steps          jsonb NOT NULL,            -- [{"node": "...", "ms": 0}]
    caller_turns   integer NOT NULL,
    agent_turns    integer NOT NULL,
    latency_p50_ms integer,
    latency_p95_ms integer,
    latency_count  integer NOT NULL DEFAULT 0,
    usage          jsonb NOT NULL,
    received_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX calls_by_flow ON calls (flow_id, started_at DESC);
CREATE INDEX calls_by_start ON calls (started_at);
