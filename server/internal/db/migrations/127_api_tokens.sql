-- +goose Up
-- API tokens (F18): a token acts as the member who created it, in one workspace (team_id NULL is
-- the personal workspace), until it expires or is revoked. Only sha256(secret) is stored. scope
-- is the optional narrowing list of {dst, allow, ports} entries; NULL means no narrowing.
CREATE TABLE paperboat.api_tokens (
    id text PRIMARY KEY,
    account_id text NOT NULL REFERENCES paperboat.users(id),
    team_id text,
    name text NOT NULL,
    secret_hash bytea NOT NULL UNIQUE,
    scope jsonb,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    expires_at timestamp with time zone NOT NULL,
    last_used_at timestamp with time zone,
    revoked_at timestamp with time zone,
    revoked_by text,
    CONSTRAINT api_tokens_secret_hash_length CHECK (octet_length(secret_hash) = 32),
    CONSTRAINT api_tokens_expiry CHECK (expires_at > created_at),
    CONSTRAINT api_tokens_scope_array CHECK (scope IS NULL OR jsonb_typeof(scope) = 'array'),
    CONSTRAINT api_tokens_revocation CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE UNIQUE INDEX api_tokens_live_name ON paperboat.api_tokens (account_id, coalesce(team_id, ''), name) WHERE revoked_at IS NULL;
CREATE INDEX api_tokens_workspace ON paperboat.api_tokens (coalesce(team_id, ''), created_at DESC);

-- +goose Down
DROP TABLE paperboat.api_tokens;
