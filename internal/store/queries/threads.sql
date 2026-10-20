-- name: UpsertThread :one
INSERT INTO threads (id, account_id, subject_norm, last_message_at, message_count)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO UPDATE SET
    last_message_at = GREATEST(threads.last_message_at, EXCLUDED.last_message_at),
    message_count = threads.message_count + 1,
    updated_at = now()
RETURNING *;

-- name: GetThread :one
SELECT * FROM threads WHERE id = $1;

-- name: ListThreadMessages :many
SELECT * FROM messages WHERE thread_id = $1 ORDER BY sent_at NULLS LAST, received_at;

-- name: IncrementThread :exec
UPDATE threads SET message_count = message_count + 1,
    last_message_at = GREATEST(last_message_at, $2), updated_at = now()
WHERE id = $1;
