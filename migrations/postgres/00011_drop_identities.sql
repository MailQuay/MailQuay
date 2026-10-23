-- +goose Up
-- Sender identities are gone. Sending always uses the account address; the
-- display name and Reply-To default live on the account, and Reply-To for a
-- message is edited in the compose form.
ALTER TABLE accounts
    ADD COLUMN default_send_name text NOT NULL DEFAULT '',
    ADD COLUMN default_reply_to text NOT NULL DEFAULT '';

UPDATE accounts a
SET default_send_name = i.name,
    default_reply_to = i.reply_to
FROM identities i
WHERE i.account_id = a.id;

ALTER TABLE drafts
    DROP COLUMN identity_account_id,
    DROP COLUMN identity_email;

ALTER TABLE outbox
    DROP COLUMN identity_account_id,
    DROP COLUMN identity_email;

DROP TABLE identities;

-- +goose Down
-- Identity names and Reply-To values are restored from the account columns.
CREATE TABLE identities (
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    email text NOT NULL,
    name text NOT NULL DEFAULT '',
    organization text NOT NULL DEFAULT '',
    reply_to text NOT NULL DEFAULT '',
    bcc text[],
    is_default boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, email)
);
CREATE UNIQUE INDEX identities_account_key ON identities (account_id);

INSERT INTO identities (account_id, email, name, reply_to, is_default)
SELECT id, email, default_send_name, default_reply_to, true
FROM accounts;

ALTER TABLE drafts
    ADD COLUMN identity_account_id uuid REFERENCES accounts (id) ON DELETE CASCADE,
    ADD COLUMN identity_email text NOT NULL DEFAULT '';
UPDATE drafts SET identity_account_id = account_id, identity_email = '';
ALTER TABLE drafts ALTER COLUMN identity_account_id SET NOT NULL;

ALTER TABLE outbox
    ADD COLUMN identity_account_id uuid REFERENCES accounts (id) ON DELETE CASCADE,
    ADD COLUMN identity_email text NOT NULL DEFAULT '';
UPDATE outbox SET identity_account_id = account_id, identity_email = '';
ALTER TABLE outbox ALTER COLUMN identity_account_id SET NOT NULL;

UPDATE drafts d SET identity_email = a.email FROM accounts a WHERE d.identity_account_id = a.id;
UPDATE outbox o SET identity_email = a.email FROM accounts a WHERE o.identity_account_id = a.id;

ALTER TABLE accounts
    DROP COLUMN default_send_name,
    DROP COLUMN default_reply_to;
