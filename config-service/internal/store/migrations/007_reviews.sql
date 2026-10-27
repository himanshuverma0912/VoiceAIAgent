-- Maker-checker: a maker submits the draft for review; a checker (a different
-- person) approves it, which publishes exactly what was submitted, or rejects
-- it with a reason. The submitted flow is copied, so later edits to the draft
-- don't change what the checker sees or publishes.
CREATE TABLE flow_reviews (
    review_id     bigserial PRIMARY KEY,
    flow_id       text NOT NULL REFERENCES flows (flow_id) ON DELETE CASCADE,
    spec          json NOT NULL,
    checksum      text NOT NULL,
    base_version  integer,                     -- live version when submitted (what the change is against)
    note          text NOT NULL DEFAULT '',    -- the maker's description of the change
    submitted_by  text NOT NULL,
    submitted_at  timestamptz NOT NULL DEFAULT now(),
    status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'approved', 'rejected', 'withdrawn', 'superseded')),
    decided_by    text,
    decided_at    timestamptz,
    decision_note text NOT NULL DEFAULT '',
    version       integer,                     -- the version an approval published
    CHECK (status = 'pending' OR decided_at IS NOT NULL)
);
-- At most one open review per flow.
CREATE UNIQUE INDEX flow_reviews_one_pending ON flow_reviews (flow_id) WHERE status = 'pending';
CREATE INDEX flow_reviews_by_flow ON flow_reviews (flow_id, review_id DESC);

-- Who made and who approved each published version.
ALTER TABLE flow_versions ADD COLUMN submitted_by text;
ALTER TABLE flow_versions ADD COLUMN review_id bigint REFERENCES flow_reviews (review_id);
