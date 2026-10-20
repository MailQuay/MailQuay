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
SELECT * FROM tasks
WHERE dismissed_at IS NULL AND (
    status IN ('pending', 'running', 'failed')
    OR (status IN ('completed', 'cancelled') AND completed_at > now() - interval '10 seconds')
)
ORDER BY CASE status WHEN 'failed' THEN 0 WHEN 'running' THEN 1 WHEN 'pending' THEN 2 ELSE 3 END, updated_at DESC;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1;

-- name: GetActiveTask :one
SELECT * FROM tasks WHERE type = $1 AND account_id IS NOT DISTINCT FROM sqlc.narg(account_id) AND status IN ('pending', 'running');

-- name: CleanupTasks :exec
DELETE FROM tasks WHERE status IN ('completed', 'cancelled') AND completed_at < now() - interval '1 day';
