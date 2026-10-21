-- +goose Up
ALTER TABLE accounts DROP COLUMN IF EXISTS color;

-- +goose Down
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS color text NOT NULL DEFAULT '#f97316';
