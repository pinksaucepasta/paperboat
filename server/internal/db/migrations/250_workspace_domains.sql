-- +goose Up
-- Workspace domains: a domain added once to a workspace (personal or team), verified once by its
-- _acme-challenge CNAME, holding one wildcard certificate that every tunnel and preview hostname
-- under it reuses. Tunnel and preview hostnames point at the workspace domain they live under.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.workspace_domains (
    id text PRIMARY KEY CHECK (id ~ '^wsd_[a-z0-9]{8,64}$'),
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    owner_team_id text,
    hostname text NOT NULL CHECK (length(hostname) BETWEEN 3 AND 253 AND hostname = lower(hostname)
        AND hostname = btrim(hostname) AND hostname !~ '[*]' AND hostname !~ '[.]$' AND hostname ~ '[.]'),
    challenge_reference text NOT NULL CHECK (length(challenge_reference) BETWEEN 1 AND 256),
    ownership_state text NOT NULL DEFAULT 'pending' CHECK (ownership_state IN ('pending', 'verified', 'failed')),
    dns_failure_code text CHECK (dns_failure_code IS NULL OR length(dns_failure_code) BETWEEN 1 AND 64),
    observed_records jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(observed_records) = 'array'),
    dns_last_checked_at timestamptz,
    dns_next_check_at timestamptz NOT NULL DEFAULT now(),
    verification_attempts integer NOT NULL DEFAULT 0 CHECK (verification_attempts >= 0),
    verified_at timestamptz,
    certificate_state text NOT NULL DEFAULT 'pending' CHECK (certificate_state IN ('pending', 'ready', 'failed')),
    certificate_failure_code text CHECK (certificate_failure_code IS NULL OR length(certificate_failure_code) BETWEEN 1 AND 128),
    certificate_attempts integer NOT NULL DEFAULT 0 CHECK (certificate_attempts >= 0),
    certificate_retry_at timestamptz,
    master_key_reference text,
    certificate_ciphertext bytea,
    private_key_ciphertext bytea,
    certificate_fingerprint bytea CHECK (certificate_fingerprint IS NULL OR octet_length(certificate_fingerprint) = 32),
    certificate_expires_at timestamptz,
    created_by_actor_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((certificate_ciphertext IS NULL) = (private_key_ciphertext IS NULL)
        AND (certificate_ciphertext IS NULL) = (certificate_expires_at IS NULL)
        AND (certificate_ciphertext IS NULL) = (certificate_fingerprint IS NULL)
        AND (certificate_ciphertext IS NULL) = (master_key_reference IS NULL))
);
CREATE UNIQUE INDEX workspace_domains_hostname ON paperboat.workspace_domains (hostname);
CREATE INDEX workspace_domains_workspace ON paperboat.workspace_domains (account_id, owner_team_id, hostname);
CREATE INDEX workspace_domains_team ON paperboat.workspace_domains (owner_team_id, hostname) WHERE owner_team_id IS NOT NULL;
CREATE INDEX workspace_domains_dns_due ON paperboat.workspace_domains (dns_next_check_at, id);
CREATE INDEX workspace_domains_certificate_due ON paperboat.workspace_domains (certificate_expires_at NULLS FIRST, id)
    WHERE ownership_state = 'verified';

-- A workspace domain's own certificate order takes the shared issuance lock.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.tunnel_certificate_issuance_lock_target_guard_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM tunnel_domains WHERE id = NEW.domain_id)
     AND NOT EXISTS (SELECT 1 FROM preview_domains WHERE id = NEW.domain_id)
     AND NOT EXISTS (SELECT 1 FROM workspace_domains WHERE id = NEW.domain_id)
     AND NOT EXISTS (SELECT 1 FROM tunnel_platform_certificate_targets WHERE id = NEW.domain_id) THEN
    RAISE EXCEPTION 'certificate issuance lock target does not exist';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.tunnel_certificate_issuance_lock_target_guard_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM tunnel_domains WHERE id = NEW.domain_id)
     AND NOT EXISTS (SELECT 1 FROM preview_domains WHERE id = NEW.domain_id)
     AND NOT EXISTS (SELECT 1 FROM tunnel_platform_certificate_targets WHERE id = NEW.domain_id) THEN
    RAISE EXCEPTION 'certificate issuance lock target does not exist';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd
DROP TABLE paperboat.workspace_domains;
