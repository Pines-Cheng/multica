-- Runs mirrored as live cards are keyed by their task.
ALTER TABLE channel_issue_message ADD COLUMN IF NOT EXISTS task_id uuid;
