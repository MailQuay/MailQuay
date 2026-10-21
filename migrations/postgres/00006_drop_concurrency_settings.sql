-- +goose Up
-- Network pool sizing is operator-set and index pools follow detected local
-- resources, so these keys no longer exist.
DELETE FROM settings WHERE key IN ('concurrency_mode', 'search_concurrency', 'index_batch_size');

-- +goose Down
-- Removed settings are not restored.
