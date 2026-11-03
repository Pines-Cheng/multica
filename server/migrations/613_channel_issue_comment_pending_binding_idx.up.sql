CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_issue_comment_pending_binding_idx ON channel_issue_comment_pending (binding_id, id);
