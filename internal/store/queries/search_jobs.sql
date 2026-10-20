-- name: EnqueueSearchJob :one
INSERT INTO search_jobs (id, message_id, op)
VALUES ($1, $2, $3)
ON CONFLICT (message_id) WHERE status IN ('pending', 'processing') DO UPDATE SET
    op = EXCLUDED.op, status = 'pending', next_attempt_at = now(), last_error = ''
RETURNING *;

-- name: ClaimSearchJobs :many
UPDATE search_jobs SET status = 'processing', lease_until = $1, updated_at = now()
WHERE id IN (
    SELECT id FROM search_jobs
    WHERE status = 'pending' AND next_attempt_at <= now()
    ORDER BY created_at
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
RETURNING *;

-- name: CompleteSearchJob :exec
DELETE FROM search_jobs WHERE id = $1;

-- name: FailSearchJob :exec
UPDATE search_jobs SET status = 'pending', attempts = attempts + 1, last_error = $2, next_attempt_at = $3, lease_until = NULL, updated_at = now() WHERE id = $1;

-- name: GiveUpSearchJob :exec
UPDATE search_jobs SET status = 'failed', attempts = attempts + 1, last_error = $2, lease_until = NULL, updated_at = now() WHERE id = $1;

-- name: ResetExpiredSearchJobs :exec
UPDATE search_jobs SET status = 'pending', lease_until = NULL, updated_at = now()
WHERE status = 'processing' AND lease_until < now();

-- name: CountPendingSearchJobs :one
SELECT count(*) FROM search_jobs WHERE status IN ('pending', 'processing');
