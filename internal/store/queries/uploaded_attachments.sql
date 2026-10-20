-- name: CreateUploadedAttachment :one
INSERT INTO uploaded_attachments (id, filename, content_type, size, sha256)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetUploadedAttachment :one
SELECT * FROM uploaded_attachments WHERE id = $1;

-- name: ListUploadedAttachments :many
SELECT * FROM uploaded_attachments WHERE id = ANY(sqlc.arg(ids)::uuid[]);
