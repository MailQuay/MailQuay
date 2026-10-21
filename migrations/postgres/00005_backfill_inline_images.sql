-- +goose Up
-- Messages cached before inline images were retained have has_attachments set
-- but no attachment rows. Force one body refetch so inline images are stored.
UPDATE messages
SET body_state = 'missing'
WHERE body_state = 'cached'
  AND has_attachments
  AND NOT local_flag
  AND preserved_at IS NULL;

-- +goose Down
SELECT 1;
