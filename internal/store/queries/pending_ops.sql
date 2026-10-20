-- name: EnqueuePendingOp :one
INSERT INTO pending_ops (id, account_id, mailbox_id, message_id, uid, kind, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListPendingOps :many
SELECT * FROM pending_ops
WHERE account_id = $1 AND status = 'pending' AND next_attempt_at <= now()
ORDER BY created_at LIMIT $2;

-- name: ClaimPendingOps :many
UPDATE pending_ops SET status = 'processing', updated_at = now()
WHERE id IN (
    SELECT id FROM pending_ops
    WHERE pending_ops.account_id = sqlc.arg(account_id) AND pending_ops.status = 'pending' AND next_attempt_at <= now()
    ORDER BY created_at
    FOR UPDATE SKIP LOCKED
    LIMIT sqlc.arg(page_limit)
)
RETURNING *;

-- name: CompletePendingOp :exec
DELETE FROM pending_ops WHERE id = $1;

-- name: FailPendingOp :exec
UPDATE pending_ops SET status = $2, attempts = attempts + 1, last_error = $3, next_attempt_at = $4, updated_at = now() WHERE id = $1;

-- name: ResetProcessingPendingOps :exec
UPDATE pending_ops SET status = 'pending', updated_at = now() WHERE status = 'processing';

-- name: DeletePendingOpsByMailbox :exec
DELETE FROM pending_ops WHERE mailbox_id = $1;

-- name: DeletePendingOpsByAccount :exec
DELETE FROM pending_ops WHERE account_id = $1;
