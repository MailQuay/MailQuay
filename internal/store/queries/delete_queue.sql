-- name: EnqueueDelete :one
INSERT INTO delete_queue (id, message_id, account_id, mailbox_id, uid, delete_after)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (message_id) WHERE status = 'pending' DO UPDATE SET delete_after = EXCLUDED.delete_after
RETURNING *;

-- name: ListDueDeletes :many
SELECT * FROM delete_queue WHERE status = 'pending' AND delete_after <= now() ORDER BY delete_after LIMIT $1;

-- name: MarkDeleteDone :exec
UPDATE delete_queue SET status = 'deleted', updated_at = now() WHERE id = $1;

-- name: MarkDeleteFailed :exec
UPDATE delete_queue SET status = 'failed', attempts = attempts + 1, last_error = $2, updated_at = now() WHERE id = $1;

-- name: MarkDeleteRetry :exec
UPDATE delete_queue SET attempts = attempts + 1, last_error = $2, updated_at = now() WHERE id = $1;

-- name: ListPendingDeletes :many
SELECT * FROM delete_queue WHERE status = 'pending' ORDER BY delete_after;
