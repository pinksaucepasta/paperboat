-- +goose Up
-- PLAN F19-02..06: team SSO and directory sync through WorkOS. WorkOS owns IdP configuration;
-- these tables mirror what Paperboat needs to show and enforce.
ALTER TABLE paperboat.teams
 ADD COLUMN workos_organization_id text UNIQUE,
 ADD COLUMN sso_required boolean NOT NULL DEFAULT false,
 ADD COLUMN sso_session_seconds integer CHECK (sso_session_seconds IS NULL OR sso_session_seconds > 0),
 ADD COLUMN sso_jit boolean NOT NULL DEFAULT false,
 ADD COLUMN directory_mapped_only boolean NOT NULL DEFAULT false,
 ADD CONSTRAINT teams_sso_required_session CHECK (NOT sso_required OR sso_session_seconds IS NOT NULL);
ALTER TABLE paperboat.team_members
 ADD COLUMN sso_verified_at timestamptz,
 ADD COLUMN sso_exempt boolean NOT NULL DEFAULT false,
 ADD COLUMN sso_bypass_recorded_at timestamptz,
 ADD COLUMN directory_user_id text;
-- A directory invitation waits for the person's first sign-in; it does not expire.
ALTER TABLE paperboat.team_invitations ADD COLUMN directory_user_id text;
CREATE UNIQUE INDEX team_members_directory_user ON paperboat.team_members(team_id, directory_user_id)
 WHERE directory_user_id IS NOT NULL AND active;

-- The organization's last read state, shown on the team Security and Directory pages.
CREATE TABLE paperboat.team_identity_state (
 team_id text PRIMARY KEY,
 domains jsonb NOT NULL DEFAULT '[]',
 connections jsonb NOT NULL DEFAULT '[]',
 directory jsonb,
 directory_groups jsonb NOT NULL DEFAULT '[]',
 directory_users integer NOT NULL DEFAULT 0,
 synced_at timestamptz,
 sync_error text CHECK (sync_error IS NULL OR length(sync_error) <= 200),
 dirty boolean NOT NULL DEFAULT true
);
-- Verified domains, unique across teams: used to restrict invitations and for JIT.
CREATE TABLE paperboat.team_sso_domains (
 domain text PRIMARY KEY CHECK (domain = lower(domain) AND length(domain) BETWEEN 3 AND 253),
 team_id text NOT NULL
);
CREATE INDEX team_sso_domains_team ON paperboat.team_sso_domains(team_id);
CREATE TABLE paperboat.team_directory_group_roles (
 team_id text NOT NULL,
 group_id text NOT NULL,
 role text NOT NULL CHECK (role IN ('admin', 'billing', 'auditor', 'member')),
 PRIMARY KEY (team_id, group_id)
);
-- People who signed in through the team's SSO without being members (JIT provisioning).
CREATE TABLE paperboat.team_jit_requests (
 team_id text NOT NULL,
 account_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (team_id, account_id)
);
CREATE TABLE paperboat.workos_event_cursor (
 singleton boolean PRIMARY KEY CHECK (singleton),
 after_event text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
-- The SSO guardrail (PLAN F19-04): with Require SSO on, a member reaches the team only after an
-- SSO sign-in within the team's SSO session length. The owner and SSO-exempt members are the
-- break-glass accounts. Non-members are never allowed.
CREATE FUNCTION paperboat.team_member_sso_ok(team text, account text) RETURNS boolean
 LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT coalesce((SELECT NOT t.sso_required OR t.owner_account=account OR m.sso_exempt
   OR m.sso_verified_at > now() - make_interval(secs => t.sso_session_seconds)
  FROM teams t JOIN team_members m ON m.team_id=t.team_id AND m.account_id=account AND m.active
  WHERE t.team_id=team), false);
$$;

-- Records an SSO sign-in for every team of the WorkOS organization the account belongs to, and
-- queues JIT provisioning where the team allows it and the email's domain is verified for it.
-- Returns the teams whose membership was verified.
CREATE FUNCTION paperboat.record_team_sso_signin(organization text, account text, email text) RETURNS SETOF text
 LANGUAGE plpgsql SET search_path TO 'paperboat', 'pg_catalog' AS $$
BEGIN
 INSERT INTO team_jit_requests(team_id, account_id)
 SELECT t.team_id, account FROM teams t JOIN team_sso_domains d ON d.team_id=t.team_id
 WHERE t.workos_organization_id=organization AND t.sso_jit AND t.deleted_at IS NULL
  AND d.domain=lower(split_part(email, '@', 2))
  AND NOT EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=t.team_id AND m.account_id=account AND m.active)
 ON CONFLICT DO NOTHING;
 RETURN QUERY UPDATE team_members m SET sso_verified_at=now() FROM teams t
  WHERE t.workos_organization_id=organization AND t.deleted_at IS NULL AND m.team_id=t.team_id
   AND m.account_id=account AND m.active
  RETURNING m.team_id;
END $$;

-- The membership guardrail every machine capability and port check uses (F18) also requires the
-- SSO sign-in (F19-04): new connections to team machines need it; open ones are untouched.
CREATE OR REPLACE FUNCTION paperboat.team_machine_bound_for(actor text, team text, machine text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT team_paid_entitled(team) AND EXISTS (
  SELECT 1 FROM teams t JOIN team_members member USING(team_id)
  JOIN team_resource_bindings b USING(team_id)
  JOIN user_machines m ON m.id=b.resource_id
  WHERE t.team_id=team AND t.deleted_at IS NULL AND member.account_id=actor AND member.active
   AND b.resource_kind='machine' AND b.active AND m.id=machine AND m.revoked_at IS NULL AND m.deleted_at IS NULL
   AND ((m.owner_team_id IS NULL AND b.owner_account=m.user_id) OR m.owner_team_id=t.team_id))
  AND team_member_sso_ok(team, actor);
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.team_machine_bound_for(actor text, team text, machine text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT team_paid_entitled(team) AND EXISTS (
  SELECT 1 FROM teams t JOIN team_members member USING(team_id)
  JOIN team_resource_bindings b USING(team_id)
  JOIN user_machines m ON m.id=b.resource_id
  WHERE t.team_id=team AND t.deleted_at IS NULL AND member.account_id=actor AND member.active
   AND b.resource_kind='machine' AND b.active AND m.id=machine AND m.revoked_at IS NULL AND m.deleted_at IS NULL
   AND ((m.owner_team_id IS NULL AND b.owner_account=m.user_id) OR m.owner_team_id=t.team_id));
$$;
-- +goose StatementEnd
DROP FUNCTION paperboat.record_team_sso_signin(text, text, text);
DROP FUNCTION paperboat.team_member_sso_ok(text, text);
DROP TABLE paperboat.workos_event_cursor;
DROP TABLE paperboat.team_jit_requests;
DROP TABLE paperboat.team_directory_group_roles;
DROP TABLE paperboat.team_sso_domains;
DROP TABLE paperboat.team_identity_state;
DROP INDEX paperboat.team_members_directory_user;
ALTER TABLE paperboat.team_invitations DROP COLUMN directory_user_id;
ALTER TABLE paperboat.team_members DROP COLUMN sso_verified_at, DROP COLUMN sso_exempt, DROP COLUMN sso_bypass_recorded_at, DROP COLUMN directory_user_id;
ALTER TABLE paperboat.teams DROP CONSTRAINT teams_sso_required_session, DROP COLUMN workos_organization_id,
 DROP COLUMN sso_required, DROP COLUMN sso_session_seconds, DROP COLUMN sso_jit, DROP COLUMN directory_mapped_only;
