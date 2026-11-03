-- name: GetFeishuLoginIdentity :one
SELECT * FROM feishu_login_identity
WHERE app_id = $1 AND open_id = $2;

-- name: CreateFeishuLoginIdentity :one
INSERT INTO feishu_login_identity (app_id, open_id, tenant_key, union_id, user_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListFeishuLoginUsersByUnionID :many
SELECT DISTINCT user_id FROM feishu_login_identity
WHERE tenant_key = $1 AND union_id = $2 AND union_id <> '';
