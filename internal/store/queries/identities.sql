-- name: ListIdentities :many
SELECT * FROM identities WHERE account_id = $1 ORDER BY is_default DESC, email;

-- name: GetIdentity :one
SELECT * FROM identities WHERE account_id = $1 AND lower(email) = lower($2);

-- name: CreateIdentity :one
INSERT INTO identities (account_id, email, name, organization, reply_to, bcc, signature_html, signature_text, signature_mode, is_default)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: UpdateIdentity :one
UPDATE identities SET name = $3, organization = $4, reply_to = $5, bcc = $6,
    signature_html = $7, signature_text = $8, signature_mode = $9, updated_at = now()
WHERE account_id = $1 AND lower(email) = lower($2)
RETURNING *;

-- name: DeleteIdentity :exec
DELETE FROM identities WHERE account_id = $1 AND lower(email) = lower($2);

-- name: ClearDefaultIdentity :exec
UPDATE identities SET is_default = false, updated_at = now() WHERE account_id = $1;

-- name: SetDefaultIdentity :exec
UPDATE identities SET is_default = true, updated_at = now() WHERE account_id = $1 AND lower(email) = lower($2);

-- name: CountIdentities :one
SELECT count(*) FROM identities WHERE account_id = $1;

-- name: GetDefaultIdentity :one
SELECT * FROM identities WHERE account_id = $1 AND is_default LIMIT 1;
