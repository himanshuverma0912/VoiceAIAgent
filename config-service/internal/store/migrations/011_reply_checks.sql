-- Call history: the reply checks that changed or stopped what the agent was
-- about to say (which check, on which step, fix or stop). Never the sentence.
ALTER TABLE calls ADD COLUMN checks jsonb NOT NULL DEFAULT '[]';
