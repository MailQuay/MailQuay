-- +goose Up
-- Deleting a message only moves it to Trash now. The provider owns Trash
-- cleanup, so the local delete queue and its retention settings are gone.
DROP TABLE IF EXISTS delete_queue;
DELETE FROM settings WHERE key IN ('delete_mode', 'delete_queue_days');

-- +goose Down
-- Queue rows and settings values are not restored.
CREATE TABLE delete_queue (
    id uuid PRIMARY KEY,
    message_id uuid NOT NULL,
    account_id uuid NOT NULL,
    mailbox_id uuid NOT NULL,
    uid bigint,
    delete_after timestamptz NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT delete_queue_status_check CHECK (status IN ('pending', 'deleted', 'failed'))
);
CREATE INDEX delete_queue_due_idx ON delete_queue (status, delete_after);
CREATE UNIQUE INDEX delete_queue_message_key ON delete_queue (message_id) WHERE status = 'pending';
