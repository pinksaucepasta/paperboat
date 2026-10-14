-- +goose Up
CREATE TABLE paperboat.terminal_browser_access_policies (
    terminal_session_id text PRIMARY KEY
        REFERENCES paperboat.user_machine_terminal_sessions(id) ON DELETE CASCADE,
    audience text NOT NULL DEFAULT 'owner'
        CHECK (audience IN ('owner', 'team', 'specific_emails', 'email_domain')),
    team_id text,
    allowed_emails text[] NOT NULL DEFAULT ARRAY[]::text[],
    email_domain text,
    interactive_emails text[] NOT NULL DEFAULT ARRAY[]::text[],
    generation bigint NOT NULL DEFAULT 1
        CHECK (generation BETWEEN 1 AND 9007199254740991),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT terminal_browser_access_policy_shape CHECK (
        cardinality(allowed_emails) <= 100
        AND cardinality(interactive_emails) <= 100
        AND array_position(allowed_emails, NULL::text) IS NULL
        AND array_position(interactive_emails, NULL::text) IS NULL
        AND (
            (audience = 'owner' AND team_id IS NULL AND cardinality(allowed_emails) = 0 AND email_domain IS NULL AND cardinality(interactive_emails) = 0)
            OR (audience = 'team' AND team_id IS NOT NULL AND cardinality(allowed_emails) = 0 AND email_domain IS NULL)
            OR (audience = 'specific_emails' AND team_id IS NULL AND cardinality(allowed_emails) BETWEEN 1 AND 100 AND email_domain IS NULL AND interactive_emails <@ allowed_emails)
            OR (audience = 'email_domain' AND team_id IS NULL AND cardinality(allowed_emails) = 0 AND email_domain IS NOT NULL)
        )
    )
);

-- Every session has a persisted default policy and a nonzero generation. This
-- gives browser tickets a stable generation even before the owner shares it.
INSERT INTO paperboat.terminal_browser_access_policies (terminal_session_id, audience)
SELECT id, 'owner' FROM paperboat.user_machine_terminal_sessions
ON CONFLICT (terminal_session_id) DO NOTHING;

-- +goose StatementBegin
CREATE FUNCTION paperboat.seed_terminal_browser_access_policy() RETURNS trigger
LANGUAGE plpgsql
SET search_path TO 'paperboat', 'pg_catalog'
AS $$
BEGIN
    INSERT INTO terminal_browser_access_policies (terminal_session_id, audience)
    VALUES (NEW.id, 'owner')
    ON CONFLICT (terminal_session_id) DO NOTHING;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER terminal_browser_access_policy_default
AFTER INSERT ON paperboat.user_machine_terminal_sessions
FOR EACH ROW EXECUTE FUNCTION paperboat.seed_terminal_browser_access_policy();

-- +goose Down
DROP TRIGGER terminal_browser_access_policy_default ON paperboat.user_machine_terminal_sessions;
DROP FUNCTION paperboat.seed_terminal_browser_access_policy();
DROP TABLE paperboat.terminal_browser_access_policies;
