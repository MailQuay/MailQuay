-- name: UpsertTask :one
INSERT INTO tasks (id, type, account_id, status, done, total, unit, message, started_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (type, account_id) WHERE status IN ('pending', 'running') DO UPDATE SET
    status = EXCLUDED.status,
    done = EXCLUDED.done,
    total = CASE WHEN EXCLUDED.total > 0 THEN EXCLUDED.total ELSE tasks.total END,
    message = EXCLUDED.message,
    started_at = COALESCE(tasks.started_at, EXCLUDED.started_at),
    updated_at = now()
RETURNING *;

-- name: RestartFailedTask :one
UPDATE tasks SET status = 'running', done = sqlc.arg(done), total = sqlc.arg(total),
    message = sqlc.arg(message), error = '', started_at = sqlc.arg(started_at),
    completed_at = NULL, updated_at = now()
WHERE id = (
    SELECT t.id FROM tasks t
    WHERE t.type = sqlc.arg(type)
      AND t.account_id IS NOT DISTINCT FROM sqlc.narg(account_id)
      AND t.status = 'failed' AND t.dismissed_at IS NULL
    ORDER BY t.updated_at DESC
    LIMIT 1
)
RETURNING *;

-- name: UpdateTaskProgress :exec
UPDATE tasks SET done = sqlc.arg(done), total = CASE WHEN sqlc.arg(total)::int > 0 THEN sqlc.arg(total)::int ELSE total END,
    message = sqlc.arg(message), status = 'running', updated_at = now()
WHERE id = sqlc.arg(id);

-- name: CompleteTask :exec
UPDATE tasks SET status = 'completed', done = total, completed_at = now(), updated_at = now() WHERE id = $1;

-- name: FailTask :exec
UPDATE tasks SET status = 'failed', error = $2, completed_at = now(), updated_at = now() WHERE id = $1;

-- name: CancelTask :exec
UPDATE tasks SET status = 'cancelled', completed_at = now(), updated_at = now() WHERE id = $1;

-- name: DismissTask :exec
UPDATE tasks SET dismissed_at = now(), updated_at = now() WHERE id = $1;

-- name: ListTasks :many
-- Active work is always listed; finished work stays visible for a day so the
-- task center can show recent completions, capped to the newest 20 rows.
SELECT * FROM (
    SELECT * FROM tasks
    WHERE dismissed_at IS NULL AND status IN ('pending', 'running', 'failed')
    UNION ALL
    SELECT * FROM (
        SELECT * FROM tasks
        WHERE dismissed_at IS NULL AND status IN ('completed', 'cancelled')
          AND completed_at > now() - interval '24 hours'
        ORDER BY updated_at DESC
        LIMIT 20
    ) AS recent
) AS listed
ORDER BY CASE status WHEN 'failed' THEN 0 WHEN 'running' THEN 1 WHEN 'pending' THEN 2 ELSE 3 END, updated_at DESC;

-- name: PurgeExpiredTasks :exec
-- Task records act as logs: each task is kept for 24 hours after it ends.
DELETE FROM tasks
WHERE (status IN ('completed', 'cancelled') AND completed_at < now() - interval '24 hours')
   OR (status = 'failed' AND updated_at < now() - interval '24 hours');

-- name: DeleteTasksByType :exec
DELETE FROM tasks
WHERE type = ANY(sqlc.arg(types)::text[])
  AND account_id IS NOT DISTINCT FROM sqlc.narg(account_id)
  AND status IN ('completed', 'cancelled');

-- name: DeleteTask :exec
DELETE FROM tasks WHERE id = $1;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1;

-- name: GetActiveTask :one
SELECT * FROM tasks WHERE type = $1 AND account_id IS NOT DISTINCT FROM sqlc.narg(account_id) AND status IN ('pending', 'running');

-- name: CleanupTasks :exec
DELETE FROM tasks WHERE status IN ('completed', 'cancelled') AND completed_at < now() - interval '1 day';
