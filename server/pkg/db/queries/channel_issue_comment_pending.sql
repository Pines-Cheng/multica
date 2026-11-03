-- name: QueueChannelIssueComment :exec
INSERT INTO channel_issue_comment_pending (
    id, workspace_id, issue_id, binding_id, installation_id,
    author_user_id, channel_message_id, message
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (channel_message_id) DO NOTHING;

-- name: ClaimChannelIssueComment :one
-- Process a binding in message order, including replies to pending images.
WITH candidate AS MATERIALIZED (
    SELECT p.channel_message_id FROM channel_issue_comment_pending p
    WHERE p.next_attempt_at <= now()
      AND (p.lease_expires_at IS NULL OR p.lease_expires_at <= now())
      AND NOT EXISTS (
          SELECT 1 FROM channel_issue_comment_pending earlier
          WHERE earlier.binding_id = p.binding_id AND earlier.id < p.id
      )
    ORDER BY p.id
    LIMIT 1 FOR UPDATE OF p SKIP LOCKED
)
UPDATE channel_issue_comment_pending p
SET lease_token = sqlc.arg(lease_token), lease_expires_at = now() + interval '2 minutes'
FROM candidate c WHERE p.channel_message_id = c.channel_message_id
RETURNING p.*;

-- name: FinishChannelIssueComment :execrows
DELETE FROM channel_issue_comment_pending
WHERE channel_message_id = $1 AND workspace_id = $2 AND lease_token = $3;

-- name: RetryChannelIssueComment :exec
UPDATE channel_issue_comment_pending
SET lease_token = NULL, lease_expires_at = NULL, next_attempt_at = now() + interval '5 seconds'
WHERE channel_message_id = $1 AND workspace_id = $2 AND lease_token = $3;
