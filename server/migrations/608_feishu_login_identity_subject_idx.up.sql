CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_feishu_login_identity_subject ON feishu_login_identity (app_id, open_id);
