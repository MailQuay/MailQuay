-- name: InsertAttachment :one
INSERT INTO attachments (id, message_id, part_path, filename, content_type, size, sha256, disposition, content_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (id) DO UPDATE SET filename = EXCLUDED.filename, sha256 = EXCLUDED.sha256
RETURNING *;

-- name: ListAttachments :many
SELECT * FROM attachments WHERE message_id = $1 ORDER BY created_at, id;

-- name: GetAttachment :one
SELECT * FROM attachments WHERE message_id = $1 AND id = $2;

-- name: DeleteAttachmentsByMessage :exec
DELETE FROM attachments WHERE message_id = $1;

-- name: DeleteAttachmentsForAccount :exec
DELETE FROM attachments WHERE message_id IN (
    SELECT id FROM messages WHERE account_id = $1 AND NOT local_flag AND preserved_at IS NULL
);

-- name: ListReferencedBlobs :many
SELECT DISTINCT sha FROM (
    SELECT raw_sha256 AS sha FROM messages WHERE raw_sha256 <> ''
    UNION SELECT raw_sha256 FROM local_archives WHERE raw_sha256 <> ''
    UNION SELECT text_sha256 FROM local_archives WHERE text_sha256 <> ''
    UNION SELECT sha256 FROM attachments WHERE sha256 <> ''
    UNION SELECT sha256 FROM uploaded_attachments
) refs;
