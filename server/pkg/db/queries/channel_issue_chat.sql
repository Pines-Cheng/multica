-- =====================
-- channel_issue_chat
-- =====================

-- name: CreateChannelIssueChat :one
-- First writer wins per (issue, channel_type): a concurrent EnsureChat that
-- lost the race gets no row back and re-reads the winner.
INSERT INTO channel_issue_chat (
    workspace_id, issue_id, root_issue_id, channel_type, chat_id,
    thread_root_message_id, steward_installation_id, created_by
) VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('root_issue_id'),
    sqlc.arg('channel_type'), sqlc.arg('chat_id'), sqlc.arg('thread_root_message_id'),
    sqlc.arg('steward_installation_id'), sqlc.arg('created_by')
)
ON CONFLICT (issue_id, channel_type) DO NOTHING
RETURNING *;

-- name: GetChannelIssueChatByIssue :one
SELECT * FROM channel_issue_chat
WHERE issue_id = sqlc.arg('issue_id')
  AND channel_type = sqlc.arg('channel_type');

-- name: SetChannelIssueChatSteward :exec
-- Hands every binding of one chat (its root issue and sub-issue topics) to
-- a new steward installation.
UPDATE channel_issue_chat
SET steward_installation_id = sqlc.arg('steward_installation_id'), updated_at = now()
WHERE channel_type = sqlc.arg('channel_type') AND chat_id = sqlc.arg('chat_id');

-- name: GetChannelIssueChatByRoute :one
-- Inbound routing: a chat-level message uses thread_root_message_id = '',
-- a message inside a topic uses the topic's root message id.
SELECT * FROM channel_issue_chat
WHERE channel_type = sqlc.arg('channel_type')
  AND chat_id = sqlc.arg('chat_id')
  AND thread_root_message_id = sqlc.arg('thread_root_message_id')
  AND status = 'active';

-- =====================
-- channel_issue_message
-- =====================

-- name: RecordChannelIssueMessage :exec
INSERT INTO channel_issue_message (
    workspace_id, issue_id, binding_id, installation_id,
    channel_message_id, direction, kind, comment_id, task_id
) VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('binding_id'),
    sqlc.arg('installation_id'), sqlc.arg('channel_message_id'),
    sqlc.arg('direction'), sqlc.arg('kind'), sqlc.narg('comment_id'), sqlc.narg('task_id')
)
ON CONFLICT (channel_message_id) DO NOTHING;

-- name: GetChannelIssueMessageByChannelID :one
SELECT * FROM channel_issue_message
WHERE channel_message_id = sqlc.arg('channel_message_id');

-- name: ListChannelIssueMessagesByComment :many
-- Every platform message mirroring one comment, oldest first: the first
-- row is the message a reply should quote; outbound rows are the cards an
-- edit or delete must patch.
SELECT * FROM channel_issue_message
WHERE comment_id = sqlc.arg('comment_id')
ORDER BY created_at ASC;

-- name: GetChannelIssueMessageByTask :one
-- The live card mirroring one agent run.
SELECT * FROM channel_issue_message
WHERE task_id = sqlc.arg('task_id') AND kind = 'task'
ORDER BY created_at ASC
LIMIT 1;

-- =====================
-- identity helpers
-- =====================

-- name: FindChannelUserBindingByUnionID :one
-- Feishu cross-installation identity: union_id is stable across the apps
-- of one developer, so a member bound on any bot of this workspace can be
-- recognised by another bot without a second binding prompt. The caller
-- re-checks membership and materializes a per-installation binding.
SELECT b.* FROM channel_user_binding b
JOIN channel_installation ci ON ci.id = b.installation_id
WHERE b.workspace_id = sqlc.arg('workspace_id')
  AND b.channel_type = sqlc.arg('channel_type')
  AND ci.channel_type = sqlc.arg('channel_type')
  AND b.config ->> 'union_id' = sqlc.arg('union_id')::text
ORDER BY b.bound_at DESC
LIMIT 1;

-- name: ListChannelUserBindingsForUsers :many
-- Channel identities for a set of members, used to invite them into a chat
-- and to render native mentions. Bindings on the preferred installation
-- sort first so their open_id is valid for that bot.
SELECT b.* FROM channel_user_binding b
JOIN channel_installation ci ON ci.id = b.installation_id
WHERE b.workspace_id = sqlc.arg('workspace_id')
  AND b.channel_type = sqlc.arg('channel_type')
  AND ci.status = 'active'
  AND b.multica_user_id = ANY(sqlc.arg('user_ids')::uuid[])
ORDER BY (b.installation_id = sqlc.arg('preferred_installation_id')) DESC, b.bound_at DESC;

-- name: GetActiveChannelInstallationByAgent :one
SELECT * FROM channel_installation
WHERE workspace_id = sqlc.arg('workspace_id')
  AND agent_id = sqlc.arg('agent_id')
  AND channel_type = sqlc.arg('channel_type')
  AND status = 'active'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetLatestCommentBySourceTask :one
-- The last comment a run wrote, summarised on its finished run card.
SELECT * FROM comment
WHERE source_task_id = sqlc.arg('source_task_id') AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT 1;
