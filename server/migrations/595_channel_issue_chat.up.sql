-- Issue <-> IM group / topic binding (docs/lark-issue-group-sync.md).
-- One row per (issue, channel_type). A root issue owns a chat at the chat
-- level (thread_root_message_id = ''); a sub-issue is a topic inside its
-- ancestor's chat, keyed by the topic's root message id. No foreign keys:
-- workspace teardown deletes these rows and every lookup re-validates the
-- issue and the steward installation.
CREATE TABLE IF NOT EXISTS channel_issue_chat (
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 workspace_id uuid NOT NULL,
 issue_id uuid NOT NULL,
 root_issue_id uuid NOT NULL,
 channel_type text NOT NULL,
 chat_id text NOT NULL,
 thread_root_message_id text NOT NULL DEFAULT '',
 steward_installation_id uuid NOT NULL,
 status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'detached')),
 created_by uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- Maps each platform message that crossed the issue <-> chat boundary to the
-- comment it mirrors. It is the loop guard (a message is mirrored once), the
-- edit target for comment updates, and the quote-reply -> parent comment map.
CREATE TABLE IF NOT EXISTS channel_issue_message (
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 workspace_id uuid NOT NULL,
 issue_id uuid NOT NULL,
 binding_id uuid NOT NULL,
 installation_id uuid NOT NULL,
 channel_message_id text NOT NULL,
 direction text NOT NULL CHECK (direction IN ('inbound', 'outbound')),
 kind text NOT NULL,
 comment_id uuid,
 created_at timestamptz NOT NULL DEFAULT now()
);
