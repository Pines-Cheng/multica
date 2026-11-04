-- name: GetIssueArtifactsInWorkspace :one
SELECT artifacts, revision FROM issue
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: SetIssueArtifact :one
UPDATE issue
SET artifacts = jsonb_set(artifacts, ARRAY[sqlc.arg('key')::text], sqlc.arg('value')::jsonb),
    revision = revision + 1, updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND artifacts -> sqlc.arg('key')::text IS DISTINCT FROM sqlc.arg('value')::jsonb
RETURNING artifacts, revision;

-- name: DeleteIssueArtifact :one
UPDATE issue
SET artifacts = artifacts - sqlc.arg('key')::text,
    revision = revision + 1, updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND artifacts ? sqlc.arg('key')::text
RETURNING artifacts, revision;
