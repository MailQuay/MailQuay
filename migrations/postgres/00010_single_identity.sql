-- +goose Up
-- Signatures are gone and each mailbox account owns exactly one sender
-- identity. Keep the default identity (or the first by address), align it with
-- the account address, and drop the rest.
ALTER TABLE identities
    DROP COLUMN signature_html,
    DROP COLUMN signature_text,
    DROP COLUMN signature_mode;

DELETE FROM settings WHERE key = 'signature_position';

CREATE TEMP TABLE keep_identity AS
SELECT DISTINCT ON (account_id) account_id, email
FROM identities
ORDER BY account_id, is_default DESC, email;

UPDATE drafts d
SET identity_email = a.email
FROM accounts a
WHERE d.identity_account_id = a.id AND d.identity_email <> a.email;

UPDATE outbox o
SET identity_email = a.email
FROM accounts a
WHERE o.identity_account_id = a.id AND o.identity_email <> a.email;

DELETE FROM identities i
USING keep_identity k
WHERE i.account_id = k.account_id AND i.email <> k.email;

UPDATE identities i
SET email = a.email,
    name = CASE WHEN i.name = '' THEN a.name ELSE i.name END,
    is_default = true,
    updated_at = now()
FROM accounts a
WHERE i.account_id = a.id;

INSERT INTO identities (account_id, email, name, is_default)
SELECT a.id, a.email, a.name, true
FROM accounts a
WHERE NOT EXISTS (SELECT 1 FROM identities i WHERE i.account_id = a.id);

DROP TABLE keep_identity;

CREATE UNIQUE INDEX identities_account_key ON identities (account_id);

-- +goose Down
DROP INDEX IF EXISTS identities_account_key;

ALTER TABLE identities
    ADD COLUMN signature_html text NOT NULL DEFAULT '',
    ADD COLUMN signature_text text NOT NULL DEFAULT '',
    ADD COLUMN signature_mode text NOT NULL DEFAULT 'all',
    ADD CONSTRAINT identities_signature_mode_check CHECK (signature_mode IN ('all', 'new', 'reply', 'forward', 'none'));
