-- +goose Up
CREATE TABLE IF NOT EXISTS uploaded_attachments (
    id uuid PRIMARY KEY,
    filename text NOT NULL DEFAULT '',
    content_type text NOT NULL DEFAULT 'application/octet-stream',
    size bigint NOT NULL DEFAULT 0,
    sha256 text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS uploaded_attachments_created_idx ON uploaded_attachments (created_at);

-- +goose Down
DROP TABLE IF EXISTS uploaded_attachments;
