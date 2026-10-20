-- name: CreateOAuthState :exec
INSERT INTO oauth_states (state, provider, verifier_enc, redirect_after, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ConsumeOAuthState :one
DELETE FROM oauth_states WHERE state = $1 AND expires_at > now() RETURNING *;

-- name: DeleteExpiredOAuthStates :exec
DELETE FROM oauth_states WHERE expires_at <= now();
