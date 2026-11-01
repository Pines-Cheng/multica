CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_issue_chat_issue_idx ON channel_issue_chat (issue_id, channel_type);
