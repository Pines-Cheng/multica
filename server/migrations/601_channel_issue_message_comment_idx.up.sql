CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_issue_message_comment_idx ON channel_issue_message (comment_id) WHERE comment_id IS NOT NULL;
