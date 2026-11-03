CREATE TABLE channel_issue_comment_pending (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    binding_id UUID NOT NULL,
    installation_id UUID NOT NULL,
    author_user_id UUID NOT NULL,
    channel_message_id TEXT NOT NULL,
    message JSONB NOT NULL,
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
