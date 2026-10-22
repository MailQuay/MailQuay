-- +goose Up
-- A local move clears the message UID until the IMAP MOVE reports the
-- destination UID, so the destination sync pass may need to adopt the row by
-- its Message-ID. The partial index keeps that lookup cheap: only rows waiting
-- for their destination UID are indexed.
CREATE INDEX IF NOT EXISTS messages_mailbox_uidless_header_idx
    ON messages (mailbox_id, message_id_header)
    WHERE uid IS NULL;

-- +goose Down
DROP INDEX IF EXISTS messages_mailbox_uidless_header_idx;
