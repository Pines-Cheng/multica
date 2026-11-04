-- name: SetCommentAssistanceStatus :one
UPDATE comment SET assistance_status = sqlc.arg('status'),
    revision = revision + CASE WHEN assistance_status <> sqlc.arg('status') THEN 1 ELSE 0 END,
    updated_at = CASE WHEN assistance_status <> sqlc.arg('status') THEN now() ELSE updated_at END
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND deleted_at IS NULL AND assistance_status <> 'none'
RETURNING *;

-- name: ListOpenAssistanceForChannelChat :many
SELECT c.*, b.thread_root_message_id, b.id AS binding_id,
       (SELECT m.channel_message_id FROM channel_issue_message m
        WHERE m.comment_id = c.id AND m.workspace_id = c.workspace_id
          AND m.binding_id = b.id AND m.direction = 'outbound'
        ORDER BY m.created_at ASC LIMIT 1) AS channel_message_id
FROM comment c
JOIN channel_issue_chat b ON b.issue_id = c.issue_id AND b.workspace_id = c.workspace_id
WHERE b.root_issue_id = sqlc.arg('root_issue_id')
  AND b.workspace_id = sqlc.arg('workspace_id')
  AND b.channel_type = sqlc.arg('channel_type') AND b.status = 'active'
  AND c.assistance_status = 'open' AND c.deleted_at IS NULL
ORDER BY c.created_at ASC, c.id ASC;
