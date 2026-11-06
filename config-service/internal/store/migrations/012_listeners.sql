-- Call history: the side listeners that acted (which listener, its kind, what
-- it did, on which step). Never what was said, never a value it kept.
ALTER TABLE calls ADD COLUMN listeners jsonb NOT NULL DEFAULT '[]';
