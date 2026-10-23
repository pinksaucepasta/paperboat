-- +goose Up
-- PLAN F21-12 (Lane X #16): a create retried with the same Idempotency-Key returns the same
-- token and its once-shown secret. The secret is held encrypted (secrets envelope, the server's
-- encryption key) only for api_tokens.retry_seconds; expired rows are deleted.
CREATE TABLE paperboat.api_token_create_requests (
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9_-]{8,128}$'),
    request_hash bytea NOT NULL,
    token_id text NOT NULL REFERENCES paperboat.api_tokens(id) ON DELETE CASCADE,
    secret_ciphertext bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, idempotency_key)
);
CREATE INDEX api_token_create_requests_created ON paperboat.api_token_create_requests(created_at);

-- +goose Down
DROP TABLE paperboat.api_token_create_requests;
