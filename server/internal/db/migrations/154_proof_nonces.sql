-- +goose Up
-- PLAN F21-13 (Lane X #16): one use per signed proof nonce, until the proof expires (at most a
-- minute). signer is machine:<id> or helper:<id>.
CREATE TABLE paperboat.proof_nonces (
    signer text NOT NULL,
    nonce text NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (signer, nonce)
);
CREATE INDEX proof_nonces_expires ON paperboat.proof_nonces(expires_at);

-- +goose Down
DROP TABLE paperboat.proof_nonces;
