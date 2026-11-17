-- +goose Up
SET LOCAL lock_timeout = '5s';
-- An operator an admin created has no password yet: their account row holds an unusable marker and this row holds
-- the single-use setup token (only its SHA-256), valid until expires_at. Setting the password deletes the row.
-- (teleport lib/auth/accountrecovery.go: a single-use token starts the flow; the secret never transits the admin.)
CREATE TABLE paperboat.operator_invitations (
    operator_id text PRIMARY KEY REFERENCES paperboat.operator_accounts(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.operator_invitations;
