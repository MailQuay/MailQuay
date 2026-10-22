-- +goose Up
-- Special folders are resolved by IMAP special-use flags, then by the
-- conventional folder names, and finally by the per-account choices stored
-- here. auto_detect_special_folders=false uses only the stored choices.
ALTER TABLE accounts
    ADD COLUMN auto_detect_special_folders boolean NOT NULL DEFAULT true,
    ADD COLUMN special_folders jsonb NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
ALTER TABLE accounts
    DROP COLUMN auto_detect_special_folders,
    DROP COLUMN special_folders;
