CREATE TABLE IF NOT EXISTS feishu_login_identity (
    app_id text NOT NULL,
    open_id text NOT NULL,
    tenant_key text NOT NULL,
    union_id text NOT NULL DEFAULT '',
    user_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
