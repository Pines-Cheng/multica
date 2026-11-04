-- name: SetChannelIssueMessageSyncState :exec
UPDATE channel_issue_message SET sync_state = sqlc.arg('sync_state')
WHERE channel_message_id = sqlc.arg('channel_message_id')
  AND workspace_id = sqlc.arg('workspace_id') AND direction = 'inbound';
