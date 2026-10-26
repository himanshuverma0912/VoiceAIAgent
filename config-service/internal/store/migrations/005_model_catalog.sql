-- The models flows may use, managed by platform admins. Flow Studio offers
-- these in its dropdowns, and a draft that names anything else has a problem
-- (so it can't be test-called or published).
--   llm        a LiteLLM alias (infra/litellm-config.yaml), e.g. voice-fast
--   stt        a Deepgram model, e.g. nova-3
--   tts_voice  a Cartesia voice id
CREATE TABLE model_catalog (
    kind       text NOT NULL CHECK (kind IN ('llm', 'stt', 'tts_voice')),
    model_id   text NOT NULL,
    label      text NOT NULL,
    notes      text NOT NULL DEFAULT '',          -- e.g. cost or when to use it
    languages  text[] NOT NULL DEFAULT '{}',      -- empty: any language
    teams      text[] NOT NULL DEFAULT '{}',      -- empty: every team
    enabled    boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text NOT NULL DEFAULT 'migration',
    PRIMARY KEY (kind, model_id)
);

-- What the agent already uses.
INSERT INTO model_catalog (kind, model_id, label, notes, languages) VALUES
    ('llm', 'voice-fast', 'Voice fast', 'The default. LiteLLM alias; see infra/litellm-config.yaml for the real model.', '{}'),
    ('stt', 'nova-3', 'Deepgram Nova-3', 'The default. Hindi, English and Hinglish.', '{}'),
    ('stt', 'nova-2', 'Deepgram Nova-2', 'Older model; a fallback if Nova-3 has trouble with a language.', '{}'),
    ('tts_voice', '9626c31c-bec5-4cca-baa8-f8ba9e84c8bc', 'Default female voice', 'Cartesia Sonic-3; the agent''s default voice.', '{hi,en}');
