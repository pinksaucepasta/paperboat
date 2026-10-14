-- +goose Up
CREATE TABLE paperboat.github_native_links (
 id text PRIMARY KEY,
 user_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
 client_session_id text NOT NULL UNIQUE REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE,
 launch_hash text NOT NULL,
 browser_hash text NOT NULL DEFAULT '',
 state_hash text NOT NULL DEFAULT '',
 state text NOT NULL CHECK(state IN ('pending','authorizing','exchanging','completed','failed','canceled')),
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL
);
CREATE INDEX github_native_links_expiry ON paperboat.github_native_links(expires_at);
