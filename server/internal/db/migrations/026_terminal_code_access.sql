-- +goose Up
-- Consolidate unreleased browser audiences without changing existing grants.
ALTER TABLE paperboat.terminal_browser_access_policies DROP CONSTRAINT terminal_browser_access_policy_shape;
ALTER TABLE paperboat.terminal_browser_access_policies DROP CONSTRAINT terminal_browser_access_policies_audience_check;
UPDATE paperboat.terminal_browser_access_policies
SET audience='specific_emails',allowed_emails=ARRAY['*@'||email_domain],generation=generation+1,updated_at=now()
WHERE audience='email_domain';
ALTER TABLE paperboat.terminal_browser_access_policies DROP COLUMN email_domain;
ALTER TABLE paperboat.terminal_browser_access_policies ADD CHECK (audience IN ('owner','team','specific_emails'));
ALTER TABLE paperboat.terminal_browser_access_policies ADD CONSTRAINT terminal_browser_access_policy_shape CHECK (
 cardinality(allowed_emails)<=100 AND cardinality(interactive_emails)<=100
 AND array_position(allowed_emails,NULL::text) IS NULL AND array_position(interactive_emails,NULL::text) IS NULL
 AND ((audience='owner' AND team_id IS NULL AND cardinality(allowed_emails)=0 AND cardinality(interactive_emails)=0)
 OR (audience='team' AND team_id IS NOT NULL AND cardinality(allowed_emails)=0)
 OR (audience='specific_emails' AND team_id IS NULL AND cardinality(allowed_emails) BETWEEN 1 AND 100))
);
CREATE TABLE paperboat.terminal_code_access (
 terminal_session_id text PRIMARY KEY REFERENCES paperboat.user_machine_terminal_sessions(id) ON DELETE CASCADE,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 9007199254740991),
 enabled boolean NOT NULL DEFAULT false,
 code_hash bytea,
 role text NOT NULL DEFAULT 'viewer' CHECK (role IN ('viewer','interactive')),
 expires_at timestamptz,
 attempts_window timestamptz NOT NULL DEFAULT now(),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 120),
 CHECK ((enabled AND code_hash IS NOT NULL AND octet_length(code_hash)=32 AND expires_at IS NOT NULL) OR (NOT enabled AND code_hash IS NULL AND expires_at IS NULL))
);
CREATE TABLE paperboat.terminal_guest_sessions (
 id text PRIMARY KEY,
 token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
 terminal_session_id text NOT NULL REFERENCES paperboat.terminal_code_access(terminal_session_id) ON DELETE CASCADE,
 code_generation bigint NOT NULL CHECK (code_generation>0),
 display_name text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 64),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 CHECK (expires_at>created_at)
);
CREATE INDEX terminal_guest_sessions_terminal_idx ON paperboat.terminal_guest_sessions(terminal_session_id);
CREATE INDEX terminal_guest_sessions_expiry_idx ON paperboat.terminal_guest_sessions(expires_at);
ALTER TABLE paperboat.browser_terminal_tickets ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE paperboat.browser_terminal_tickets ALTER COLUMN browser_session_id DROP NOT NULL;
ALTER TABLE paperboat.browser_terminal_tickets ADD COLUMN guest_session_id text REFERENCES paperboat.terminal_guest_sessions(id) ON DELETE CASCADE;
ALTER TABLE paperboat.browser_terminal_tickets ADD CONSTRAINT browser_terminal_ticket_principal CHECK (
 (user_id IS NOT NULL AND browser_session_id IS NOT NULL AND guest_session_id IS NULL)
 OR (user_id IS NULL AND browser_session_id IS NULL AND guest_session_id IS NOT NULL AND role IN ('viewer','interactive'))
);
CREATE INDEX browser_terminal_tickets_guest_idx ON paperboat.browser_terminal_tickets(guest_session_id) WHERE guest_session_id IS NOT NULL;

-- +goose Down
-- Account policy consolidation cannot be reversed without discarding grants.
-- Use the previous release with a restored database, rather than weakening access.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Terminal guest/access migration requires database restore for rollback'; END $$;
-- +goose StatementEnd
