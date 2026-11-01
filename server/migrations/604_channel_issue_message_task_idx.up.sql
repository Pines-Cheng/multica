CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_issue_message_task_idx ON channel_issue_message (task_id) WHERE task_id IS NOT NULL;
