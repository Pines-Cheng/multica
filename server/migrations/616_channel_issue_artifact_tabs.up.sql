ALTER TABLE channel_issue_chat ADD COLUMN artifact_tabs JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(artifact_tabs) = 'object');
