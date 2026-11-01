-- Whether assigning an issue to this squad (or @-mentioning it) opens a
-- Feishu group for the issue automatically (docs/lark-issue-group-sync.md).
-- On by default, existing squads included.
ALTER TABLE squad ADD COLUMN IF NOT EXISTS auto_create_channel_chat boolean NOT NULL DEFAULT true;
