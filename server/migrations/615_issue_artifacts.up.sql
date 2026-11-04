ALTER TABLE issue ADD COLUMN artifacts JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(artifacts) = 'object' AND octet_length(artifacts::text) <= 16384);
