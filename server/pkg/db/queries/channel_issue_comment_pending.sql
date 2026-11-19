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
    WHERE p.state IN ('pending', 'dispatching', 'feedback') AND p.next_attempt_at <= now()
      AND (p.lease_expires_at IS NULL OR p.lease_expires_at <= now())
      AND NOT EXISTS (
          SELECT 1 FROM channel_issue_comment_pending earlier
          WHERE earlier.binding_id = p.binding_id AND earlier.id < p.id
            AND earlier.state IN ('pending', 'dispatching')
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

-- name: BindChannelIssueComment :execrows
UPDATE channel_issue_comment_pending
SET comment_id = sqlc.arg(comment_id), comment_content = sqlc.arg(content), state = 'dispatching'
WHERE channel_message_id = sqlc.arg(message_id) AND workspace_id = sqlc.arg(workspace_id)
  AND lease_token = sqlc.arg(lease_token) AND state = 'pending';

-- name: SetChannelIssueCommentOutcome :execrows
UPDATE channel_issue_comment_pending
SET dispatch_outcomes = sqlc.arg(outcomes), failure_code = sqlc.arg(failure_code), state = 'feedback'
WHERE channel_message_id = sqlc.arg(message_id) AND workspace_id = sqlc.arg(workspace_id)
  AND lease_token = sqlc.arg(lease_token);

-- name: SetChannelIssueCommentReceipt :execrows
UPDATE channel_issue_comment_pending
SET receipt_message_id = sqlc.arg(receipt_message_id)
WHERE channel_message_id = sqlc.arg(message_id) AND workspace_id = sqlc.arg(workspace_id)
  AND lease_token = sqlc.arg(lease_token);

-- name: CompleteChannelIssueComment :execrows
UPDATE channel_issue_comment_pending
SET state = sqlc.arg(state), lease_token = NULL, lease_expires_at = NULL
WHERE channel_message_id = sqlc.arg(message_id) AND workspace_id = sqlc.arg(workspace_id)
  AND lease_token = sqlc.arg(lease_token);

-- name: GetChannelIssueCommentRequest :one
SELECT * FROM channel_issue_comment_pending
WHERE channel_message_id = $1 AND workspace_id = $2;

-- name: RestartChannelIssueComment :execrows
-- Consuming the command and restarting its original request are atomic.
WITH command AS (
    UPDATE channel_issue_comment_pending cmd
    SET state = 'completed', lease_expires_at = NULL
    WHERE cmd.channel_message_id = sqlc.arg(command_message_id)
      AND cmd.workspace_id = sqlc.arg(workspace_id) AND cmd.lease_token = sqlc.arg(lease_token)
      AND cmd.state = 'pending'
    RETURNING cmd.binding_id, cmd.author_user_id
)
UPDATE channel_issue_comment_pending p
SET state = 'pending', failure_code = '', attempt = attempt + 1,
    lease_token = NULL, lease_expires_at = NULL, next_attempt_at = now()
FROM command c
WHERE p.channel_message_id = sqlc.arg(original_message_id)
  AND p.workspace_id = sqlc.arg(workspace_id) AND p.binding_id = c.binding_id
  AND p.author_user_id = c.author_user_id AND p.state = 'blocked'
  AND p.failure_code NOT IN ('dispatch_unknown', 'comment_changed', 'retry_not_allowed');

-- name: StartChannelIssueCommentRetry :execrows
UPDATE channel_issue_comment_pending SET state = 'dispatching'
WHERE channel_message_id = $1 AND workspace_id = $2 AND lease_token = $3 AND state = 'pending';

-- name: RetryChannelIssueComment :exec
UPDATE channel_issue_comment_pending
SET lease_token = NULL, lease_expires_at = NULL, next_attempt_at = now() + interval '5 seconds'
WHERE channel_message_id = $1 AND workspace_id = $2 AND lease_token = $3;
