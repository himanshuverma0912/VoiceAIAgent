-- Where Flow Studio draws each node on the canvas: {"positions": {"<node id>": {"x": 0, "y": 0}}}.
-- Kept next to the flow, not inside it, so the flow the agent runs stays pure.
ALTER TABLE flows ADD COLUMN layout json;
