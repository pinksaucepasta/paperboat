-- +goose Up
CREATE TABLE paperboat.browser_terminal_tickets (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    user_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    browser_session_id text NOT NULL REFERENCES paperboat.sessions(id) ON DELETE CASCADE,
    terminal_session_id text NOT NULL REFERENCES paperboat.user_machine_terminal_sessions(id) ON DELETE CASCADE,
    origin text NOT NULL,
    role text NOT NULL CHECK (role IN ('owner', 'viewer', 'interactive')),
    policy_generation bigint NOT NULL CHECK (policy_generation > 0),
    team_generation bigint NOT NULL DEFAULT 0 CHECK (team_generation >= 0),
    membership_generation bigint NOT NULL DEFAULT 0 CHECK (membership_generation >= 0),
    binding_generation bigint NOT NULL DEFAULT 0 CHECK (binding_generation >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    CHECK (expires_at > created_at)
);
CREATE INDEX browser_terminal_tickets_expiry_idx ON paperboat.browser_terminal_tickets (expires_at);

-- +goose Down
DROP TABLE paperboat.browser_terminal_tickets;
