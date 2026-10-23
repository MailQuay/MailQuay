-- name: CreateOutbox :one
INSERT INTO outbox (id, account_id, draft_id, message_id_header,
    status, scheduled_at, next_attempt_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetOutbox :one
SELECT * FROM outbox WHERE id = $1;

-- name: ListOutbox :many
SELECT * FROM outbox WHERE status IN ('scheduled', 'pending', 'sending', 'failed', 'copy_failed')
ORDER BY COALESCE(scheduled_at, created_at), created_at;

-- name: ClaimDueOutbox :many
UPDATE outbox SET status = 'sending', lease_until = $1, attempts = attempts + 1, updated_at = now()
WHERE id IN (
    SELECT id FROM outbox
    WHERE (status = 'pending' OR (status = 'scheduled' AND scheduled_at <= now()) OR
           (status = 'sending' AND lease_until < now()))
      AND next_attempt_at <= now()
    ORDER BY COALESCE(scheduled_at, next_attempt_at)
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
RETURNING *;

-- name: MarkOutboxSent :exec
UPDATE outbox SET status = 'sent', sent_at = now(), lease_until = NULL, last_error = '', updated_at = now()
WHERE id = $1;

-- name: MarkOutboxCopyFailed :exec
UPDATE outbox SET status = 'copy_failed', last_error = $2, lease_until = NULL, updated_at = now() WHERE id = $1;

-- name: MarkOutboxFailed :exec
UPDATE outbox SET status = 'failed', last_error = $2, lease_until = NULL, updated_at = now() WHERE id = $1;

-- name: RetryOutbox :exec
UPDATE outbox SET status = 'pending', next_attempt_at = $2, last_error = $3, updated_at = now() WHERE id = $1;

-- name: CancelOutbox :exec
UPDATE outbox SET status = 'cancelled', updated_at = now() WHERE id = $1 AND status IN ('scheduled', 'pending', 'failed');

-- name: RescheduleOutbox :one
UPDATE outbox SET scheduled_at = $2, status = 'scheduled', next_attempt_at = $2, updated_at = now()
WHERE id = $1 AND status IN ('scheduled', 'pending', 'failed')
RETURNING *;

-- name: DeleteOutboxByDraft :exec
DELETE FROM outbox WHERE draft_id = $1 AND status IN ('scheduled', 'pending', 'failed', 'cancelled');

-- name: RecoverStuckOutbox :exec
UPDATE outbox SET status = 'pending', lease_until = NULL, updated_at = now()
WHERE status = 'sending' AND lease_until < now();
