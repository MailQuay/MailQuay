-- name: CreateDraft :one
INSERT INTO drafts (id, account_id, identity_account_id, identity_email, in_reply_to, headers,
    body_text, body_html, attachments, revision, content_hash, origin, sync_state, status,
    remote_uid, remote_uidvalidity)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING *;

-- name: GetDraft :one
SELECT * FROM drafts WHERE id = $1;

-- name: ListDrafts :many
SELECT * FROM drafts WHERE status <> 'sent'
  AND (sqlc.narg(account_id)::uuid IS NULL OR account_id = sqlc.narg(account_id)::uuid)
ORDER BY updated_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: UpdateDraft :one
UPDATE drafts SET identity_account_id = $2, identity_email = $3, in_reply_to = $4,
    headers = $5, body_text = $6, body_html = $7, attachments = $8,
    revision = revision + 1, content_hash = $9, status = 'editing', sync_state = 'local', last_error = '', updated_at = now()
WHERE id = $1 AND revision = $10
RETURNING *;

-- name: DeleteDraft :exec
DELETE FROM drafts WHERE id = $1;

-- name: SetDraftStatus :exec
UPDATE drafts SET status = $2, last_error = $3, updated_at = now() WHERE id = $1;

-- name: SetDraftSent :exec
UPDATE drafts SET status = 'sent', sent_message_id = $2, updated_at = now() WHERE id = $1;

-- name: SetDraftRemote :exec
UPDATE drafts SET remote_uid = $2, remote_uidvalidity = $3, sync_state = $4, updated_at = now() WHERE id = $1;

-- name: SetDraftSyncState :exec
UPDATE drafts SET sync_state = $2, updated_at = now() WHERE id = $1;

-- name: ListDraftsToPush :many
SELECT * FROM drafts WHERE sync_state = 'local' AND status = 'editing' AND updated_at < $1;
