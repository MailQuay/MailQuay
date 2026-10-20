-- name: UpsertMailbox :one
INSERT INTO mailboxes (id, account_id, path, name, delimiter, role, attributes, local_only, sync_mode, uidvalidity, uidnext, highestmodseq)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (account_id, path) DO UPDATE SET
    name = EXCLUDED.name, delimiter = EXCLUDED.delimiter, role = EXCLUDED.role,
    attributes = EXCLUDED.attributes,
    uidvalidity = COALESCE(EXCLUDED.uidvalidity, mailboxes.uidvalidity),
    uidnext = GREATEST(COALESCE(EXCLUDED.uidnext, 0), COALESCE(mailboxes.uidnext, 0)),
    highestmodseq = GREATEST(COALESCE(EXCLUDED.highestmodseq, 0), COALESCE(mailboxes.highestmodseq, 0)),
    updated_at = now()
RETURNING *;

-- name: ListMailboxes :many
SELECT * FROM mailboxes WHERE account_id = $1 ORDER BY path;

-- name: ListActiveMailboxes :many
SELECT m.* FROM mailboxes m JOIN accounts a ON a.id = m.account_id
WHERE a.deleted_at IS NULL AND m.sync_mode <> 'off'
ORDER BY m.account_id, m.path;

-- name: GetMailbox :one
SELECT * FROM mailboxes WHERE id = $1;

-- name: GetMailboxByPath :one
SELECT * FROM mailboxes WHERE account_id = $1 AND path = $2;

-- name: GetMailboxByRole :one
SELECT * FROM mailboxes WHERE account_id = $1 AND role = $2 ORDER BY created_at LIMIT 1;

-- name: UpdateMailboxState :exec
UPDATE mailboxes SET
    uidvalidity = COALESCE($2, uidvalidity), uidnext = COALESCE($3, uidnext),
    highestmodseq = COALESCE($4, highestmodseq), sync_state = $5,
    last_synced_at = COALESCE($6, last_synced_at), last_error = $7, updated_at = now()
WHERE id = $1;

-- name: UpdateMailboxCounts :exec
UPDATE mailboxes SET total = $2, unread = $3, updated_at = now() WHERE id = $1;

-- name: SetMailboxSyncMode :exec
UPDATE mailboxes SET sync_mode = $2, updated_at = now() WHERE id = $1;

-- name: ResetMailboxUIDs :exec
UPDATE mailboxes SET uidvalidity = NULL, uidnext = NULL, highestmodseq = NULL, total = 0, unread = 0, updated_at = now() WHERE id = $1;

-- name: DeleteMailboxesByAccount :exec
DELETE FROM mailboxes WHERE account_id = $1;
