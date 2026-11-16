-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Managed SSH user certificate authorities, one signing CA per owner account (the machine's user_id, immutable,
-- like machine_ssh_host_key_owners in 238). Hosts trust every row of their owner account; the server signs with
-- the active one. Rotation adds a pending CA first, promotes it once hosts had time to fetch it, and keeps the old
-- one as retiring until every certificate it signed has expired.
CREATE TABLE paperboat.managed_ssh_user_cas (
    id text PRIMARY KEY,
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    state text NOT NULL CHECK (state IN ('pending', 'active', 'retiring')),
    public_key text NOT NULL CHECK (public_key LIKE 'ssh-ed25519 %' AND length(public_key) <= 256),
    private_key_ciphertext bytea NOT NULL CHECK (length(private_key_ciphertext) <= 1024),
    created_at timestamptz NOT NULL,
    state_changed_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX managed_ssh_user_cas_one_per_state ON paperboat.managed_ssh_user_cas (account_id, state);

-- Which CLI session holds a still-valid certificate for which machine. A machine refuses the certificates of holders
-- that lost access (revoked session, withdrawn grant, removed member) through its revocation token, so a row is kept
-- until its certificate expires, never cascaded away early: no foreign keys.
CREATE TABLE paperboat.managed_ssh_certificate_holders (
    machine_id text NOT NULL,
    cli_client_session_id text NOT NULL,
    user_id text NOT NULL,
    valid_before timestamptz NOT NULL,
    PRIMARY KEY (machine_id, cli_client_session_id)
);
CREATE INDEX managed_ssh_certificate_holders_session ON paperboat.managed_ssh_certificate_holders (cli_client_session_id);
CREATE INDEX managed_ssh_certificate_holders_valid_before ON paperboat.managed_ssh_certificate_holders (valid_before);

-- +goose Down
DROP TABLE paperboat.managed_ssh_certificate_holders;
DROP TABLE paperboat.managed_ssh_user_cas;
