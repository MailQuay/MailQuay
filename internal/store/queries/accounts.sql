-- name: CreateAccount :one
INSERT INTO accounts (
    id, name, email, color, provider, username, auth_type, password_enc,
    oauth_provider, oauth_scopes, oauth_refresh_enc, receive_protocol,
    imap_addr, imap_security, pop3_addr, pop3_security, smtp_addr, smtp_security,
    smtp_helo_hostname, sync_enabled, initial_sync_range, download_scope,
    initial_sync_cutoff_at, sent_copy_mode, pop3_leave_on_server, pop3_delete_on_server
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18,
    $19, $20, $21, $22,
    $23, $24, $25, $26
)
RETURNING *;

-- name: ListAccounts :many
SELECT * FROM accounts WHERE deleted_at IS NULL ORDER BY created_at, id;

-- name: ListActiveAccounts :many
SELECT * FROM accounts WHERE deleted_at IS NULL AND sync_enabled ORDER BY created_at, id;

-- name: GetAccount :one
SELECT * FROM accounts WHERE id = $1;

-- name: UpdateAccount :one
UPDATE accounts SET
    name = $2, email = $3, color = $4,
    username = $5, auth_type = $6, password_enc = $7,
    oauth_provider = $8, oauth_scopes = $9, oauth_refresh_enc = $10,
    receive_protocol = $11, imap_addr = $12, imap_security = $13,
    pop3_addr = $14, pop3_security = $15, smtp_addr = $16, smtp_security = $17,
    smtp_helo_hostname = $18, sync_enabled = $19,
    initial_sync_range = $20, download_scope = $21, initial_sync_cutoff_at = $22,
    sent_copy_mode = $23, pop3_leave_on_server = $24, pop3_delete_on_server = $25,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SoftDeleteAccount :exec
UPDATE accounts SET deleted_at = now(), sync_enabled = false, updated_at = now() WHERE id = $1;

-- name: HardDeleteAccount :exec
DELETE FROM accounts WHERE id = $1;

-- name: UpdateAccountOAuth :exec
UPDATE accounts SET oauth_provider = $2, oauth_scopes = $3, oauth_refresh_enc = $4, updated_at = now() WHERE id = $1;

-- name: SetAccountSyncEnabled :exec
UPDATE accounts SET sync_enabled = $2, updated_at = now() WHERE id = $1;

-- name: CountAccounts :one
SELECT count(*) FROM accounts WHERE deleted_at IS NULL;

-- name: SetAccountState :exec
UPDATE accounts SET sync_state = $2, last_error = $3, updated_at = now() WHERE id = $1;

-- name: ListDeletedAccounts :many
SELECT * FROM accounts WHERE deleted_at IS NOT NULL ORDER BY deleted_at;
