CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_feishu_login_identity_union ON feishu_login_identity (tenant_key, union_id) WHERE union_id <> '';
