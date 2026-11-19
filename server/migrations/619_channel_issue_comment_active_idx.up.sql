CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_issue_comment_active_idx ON channel_issue_comment_pending (id) WHERE state IN ('pending', 'dispatching', 'feedback');
