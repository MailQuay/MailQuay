-- name: InsertArchive :one
INSERT INTO local_archives (
    id, origin_account_id, origin_account_email, origin_mailbox_path, message_id_header,
    subject, from_addr, to_addrs, cc_addrs, bcc_addrs, sent_at, received_at, size,
    raw_sha256, text_sha256, has_attachments, attachments
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10, $11, $12, $13,
    $14, $15, $16, $17
)
ON CONFLICT (raw_sha256) DO UPDATE SET subject = local_archives.subject
RETURNING *;

-- name: GetArchive :one
SELECT * FROM local_archives WHERE id = $1;

-- name: GetArchiveByRaw :one
SELECT * FROM local_archives WHERE raw_sha256 = $1;

-- name: ListArchives :many
SELECT * FROM local_archives
WHERE (sqlc.narg(cursor_archived)::timestamptz IS NULL OR (archived_at, id) < (sqlc.narg(cursor_archived)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY archived_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListAllArchivesForIndex :many
SELECT * FROM local_archives;
