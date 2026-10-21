-- +goose Up
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS smtp_username text,
    ADD COLUMN IF NOT EXISTS smtp_password_enc bytea;

-- +goose Down
ALTER TABLE accounts
    DROP COLUMN IF EXISTS smtp_password_enc,
    DROP COLUMN IF EXISTS smtp_username;
