CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_issue_comment_blocker_idx ON channel_issue_comment_pending (binding_id, id) WHERE state IN ('pending', 'dispatching');
