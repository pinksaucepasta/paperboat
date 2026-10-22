-- +goose Up
-- Workspace policy (PLAN F18). One policy file per workspace replaces the per-machine team
-- grants (team_machine_grants) and the team setting allow_private_access. The file text is
-- stored as written; its enforcement form (shared/policy Stored) is stored as rows whose
-- subjects (role, every member, group) and targets (every machine, tag) stay symbolic, so the
-- check below resolves them per request and members joining or machines being shared need no
-- recompilation. A deny row wins over allow rows; private_access is decided per port.
CREATE TABLE paperboat.workspace_policies (
    id bigserial PRIMARY KEY,
    team_id text REFERENCES paperboat.teams(team_id) ON DELETE CASCADE,
    owner_account text REFERENCES paperboat.users(id) ON DELETE CASCADE,
    version bigint NOT NULL CHECK (version BETWEEN 1 AND 9007199254740991),
    source text NOT NULL,
    digest text NOT NULL,
    allow_private_access boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text,
    updated_with_api_token text,
    message text NOT NULL DEFAULT '',
    CHECK ((team_id IS NULL) <> (owner_account IS NULL)),
    CHECK (team_id IS NOT NULL OR NOT allow_private_access)
);
CREATE UNIQUE INDEX workspace_policies_team ON paperboat.workspace_policies(team_id) WHERE team_id IS NOT NULL;
CREATE UNIQUE INDEX workspace_policies_owner ON paperboat.workspace_policies(owner_account) WHERE owner_account IS NOT NULL;

CREATE TABLE paperboat.workspace_policy_versions (
    policy_id bigint NOT NULL REFERENCES paperboat.workspace_policies(id) ON DELETE CASCADE,
    version bigint NOT NULL,
    kind text NOT NULL CHECK (kind IN ('migrated', 'saved', 'restored', 'rewritten')),
    source text NOT NULL,
    digest text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    author text,
    api_token text,
    message text NOT NULL DEFAULT '',
    gained integer NOT NULL DEFAULT 0,
    lost integer NOT NULL DEFAULT 0,
    PRIMARY KEY (policy_id, version)
);

CREATE TABLE paperboat.workspace_policy_rules (
    policy_id bigint NOT NULL REFERENCES paperboat.workspace_policies(id) ON DELETE CASCADE,
    rule integer NOT NULL CHECK (rule >= 0),
    deny boolean NOT NULL,
    subject_kind text NOT NULL CHECK (subject_kind IN ('all', 'role', 'account', 'group')),
    subject_id text NOT NULL,
    target_kind text NOT NULL CHECK (target_kind IN ('all_machines', 'machine', 'tag', 'preview', 'tunnel', 'env')),
    target_id text NOT NULL,
    action text NOT NULL,
    ports int4range NOT NULL
);
CREATE INDEX workspace_policy_rules_lookup ON paperboat.workspace_policy_rules(policy_id, action);

CREATE TABLE paperboat.workspace_policy_groups (
    policy_id bigint NOT NULL REFERENCES paperboat.workspace_policies(id) ON DELETE CASCADE,
    group_name text NOT NULL,
    account_id text NOT NULL,
    PRIMARY KEY (policy_id, group_name, account_id)
);

CREATE TABLE paperboat.workspace_policy_tags (
    policy_id bigint NOT NULL REFERENCES paperboat.workspace_policies(id) ON DELETE CASCADE,
    tag text NOT NULL,
    machine_id text NOT NULL,
    PRIMARY KEY (policy_id, tag, machine_id)
);
CREATE INDEX workspace_policy_tags_machine ON paperboat.workspace_policy_tags(machine_id);

-- Version 1 of every team's policy is its current grants, with identical access: an
-- all_members grant becomes a role:member rule, a selected_member grant a user rule, on that
-- machine. The stored file names machines and members by ID (machine:#<id>, user:#<account>;
-- the server shows current names), so renames never change what a stored rule means.
-- Inactive grants, grants on unbound machines and grants of inactive members are dropped:
-- today they allow nothing, and member departure and unsharing already deactivate them.
CREATE TEMPORARY TABLE policy_migration ON COMMIT DROP AS
SELECT g.team_id, g.machine_id, g.audience, g.account_id,
       ARRAY(SELECT c FROM unnest(g.capabilities) c ORDER BY c) AS capabilities,
       (row_number() OVER (PARTITION BY g.team_id ORDER BY g.machine_id, g.audience, g.account_id) - 1)::integer AS rule
FROM paperboat.team_machine_grants g
JOIN paperboat.team_resource_bindings b ON b.team_id = g.team_id AND b.resource_kind = 'machine' AND b.resource_id = g.machine_id AND b.active
JOIN paperboat.user_machines m ON m.id = g.machine_id AND m.deleted_at IS NULL
WHERE g.active AND (g.audience = 'all_members' OR EXISTS(
  SELECT 1 FROM paperboat.team_members member WHERE member.team_id = g.team_id AND member.account_id = g.account_id AND member.active));

INSERT INTO paperboat.workspace_policies(team_id, version, source, digest, allow_private_access, message)
SELECT t.team_id, 1, src.source, 'sha256:' || encode(sha256(convert_to(src.source, 'UTF8')), 'hex'), t.allow_private_access,
       'Created from the machine access settings'
FROM paperboat.teams t
CROSS JOIN LATERAL (
  SELECT jsonb_pretty(jsonb_strip_nulls(jsonb_build_object(
    'allow_private_access', t.allow_private_access,
    'rules', (SELECT jsonb_agg(jsonb_build_object(
        'src', jsonb_build_array(CASE WHEN p.audience = 'all_members' THEN 'role:member' ELSE 'user:#' || p.account_id END),
        'dst', jsonb_build_array('machine:#' || p.machine_id),
        'allow', to_jsonb(p.capabilities)) ORDER BY p.rule)
      FROM policy_migration p WHERE p.team_id = t.team_id)))) AS source
) src
WHERE t.deleted_at IS NULL;

INSERT INTO paperboat.workspace_policy_versions(policy_id, version, kind, source, digest, message)
SELECT id, 1, 'migrated', source, digest, message FROM paperboat.workspace_policies;

INSERT INTO paperboat.workspace_policy_rules(policy_id, rule, deny, subject_kind, subject_id, target_kind, target_id, action, ports)
SELECT w.id, p.rule, false,
       CASE WHEN p.audience = 'all_members' THEN 'role' ELSE 'account' END,
       CASE WHEN p.audience = 'all_members' THEN 'member' ELSE p.account_id END,
       'machine', p.machine_id, c,
       CASE WHEN c = 'private_access' THEN int4range(1024, 65536) ELSE int4range(1, 65536) END
FROM policy_migration p
JOIN paperboat.workspace_policies w ON w.team_id = p.team_id
CROSS JOIN LATERAL unnest(p.capabilities) c;

-- The ports the team policy's rules give an actor for one capability on one team machine: the
-- union of matching allow rows minus the union of matching deny rows. Empty = denied. Non-port
-- capabilities are stored with every port, so "not empty" is the answer for them.
-- shared/policy Compiled.Decide is the same evaluation in Go (TestPolicySQLMatchesGo).
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_policy_rule_ports(actor text, team text, machine text, capability text) RETURNS int4multirange
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH p AS (SELECT id FROM workspace_policies WHERE team_id = team),
 actor_role AS (
  SELECT CASE WHEN t.owner_account = actor THEN 'owner' ELSE member.role END AS role
  FROM teams t JOIN team_members member USING (team_id)
  WHERE t.team_id = team AND member.account_id = actor AND member.active
 ), matching AS (
  SELECT r.deny, r.ports FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id CROSS JOIN actor_role a
  WHERE r.action = capability
   AND (r.subject_kind = 'all'
     OR (r.subject_kind = 'account' AND r.subject_id = actor)
     OR (r.subject_kind = 'role' AND (r.subject_id = 'member' OR (r.subject_id = 'admin' AND a.role IN ('owner', 'admin')) OR (r.subject_id = 'owner' AND a.role = 'owner')))
     OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id = p.id AND g.group_name = r.subject_id AND g.account_id = actor)))
   AND (r.target_kind = 'all_machines'
     OR (r.target_kind = 'machine' AND r.target_id = machine)
     OR (r.target_kind = 'tag' AND EXISTS(SELECT 1 FROM workspace_policy_tags tg WHERE tg.policy_id = p.id AND tg.tag = r.target_id AND tg.machine_id = machine)))
 )
 SELECT coalesce((SELECT range_agg(ports) FROM matching WHERE NOT deny), '{}'::int4multirange)
      - coalesce((SELECT range_agg(ports) FROM matching WHERE deny), '{}'::int4multirange);
$$;
-- +goose StatementEnd

-- The same with the team switch: no private_access while allow_private_access is false.
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_policy_ports(actor text, team text, machine text, capability text) RETURNS int4multirange
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT CASE WHEN capability = 'private_access' AND NOT coalesce((SELECT allow_private_access FROM workspace_policies WHERE team_id = team), false)
  THEN '{}'::int4multirange ELSE workspace_policy_rule_ports(actor, team, machine, capability) END;
$$;
-- +goose StatementEnd

-- The guardrails of the old check stay exactly as they were; only the team_machine_grants
-- lookup is replaced by the policy.
-- +goose StatementBegin
CREATE FUNCTION paperboat.team_machine_bound_for(actor text, team text, machine text) RETURNS boolean
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.team_machine_capability_allowed(actor text, team text, machine text, capability text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT capability=ANY(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access'])
  AND team_machine_bound_for(actor, team, machine)
  AND NOT isempty(workspace_policy_ports(actor, team, machine, capability));
$$;
-- +goose StatementEnd

-- One port of private access to a team machine.
-- +goose StatementBegin
CREATE FUNCTION paperboat.team_machine_port_allowed(actor text, team text, machine text, port integer) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT port BETWEEN 1 AND 65535 AND team_machine_bound_for(actor, team, machine)
  AND workspace_policy_ports(actor, team, machine, 'private_access') @> port;
$$;
-- +goose StatementEnd

-- Private access: the team branch asks the policy for this exact port (it carries the team
-- switch and any port rules); the personal branch is unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_service_authorities(actor text, machine text, port integer) RETURNS TABLE(source_kind text, source_id text, source_generation bigint, team_id text)
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH host AS (
 SELECT m.* FROM user_machines m JOIN users owner ON owner.id=m.user_id AND owner.status='active'
 WHERE m.id=machine AND port BETWEEN 1 AND 65535 AND m.revoked_at IS NULL AND m.deleted_at IS NULL
 AND m.state NOT IN ('pending','revoked','deleted') AND m.seat_state='occupied'
 AND EXISTS(SELECT 1 FROM users u WHERE u.id=actor AND u.status='active')
 ), explicit AS (
 SELECT 'lazy_policy'::text kind,p.id,p.generation,p.account_id,p.access_mode,NULL::text owner_team_id FROM host m JOIN lazy_access_policies p ON p.machine_id=m.id
 WHERE p.authorization_source='explicit' AND p.installation_generation=m.installation_generation AND p.deleted_at IS NULL AND p.expires_at>now()
 AND p.target_address IN ('127.0.0.1:'||port,'[::1]:'||port)
 UNION ALL
 SELECT 'tunnel',t.id,r.generation,t.account_id,t.access_mode,t.owner_team_id FROM host m JOIN tunnels t ON t.created_by_host_id=m.id JOIN tunnel_routes r ON r.tunnel_id=t.id
 WHERE t.account_id=m.user_id AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())
 AND r.protocol='private_tcp' AND r.desired_state='active' AND r.deleted_at IS NULL AND r.origin_address IN ('127.0.0.1:'||port,'[::1]:'||port)
 )
 SELECT e.kind,e.id,e.generation,NULL::text FROM explicit e CROSS JOIN host m
 WHERE m.owner_team_id IS NULL AND m.user_id=actor AND e.account_id=m.user_id AND e.access_mode='private' AND e.owner_team_id IS NULL
 UNION ALL
 SELECT e.kind,e.id,e.generation,b.team_id FROM explicit e CROSS JOIN host m
 JOIN team_resource_bindings b ON b.resource_kind=e.kind AND b.resource_id=e.id AND b.owner_account=m.user_id AND b.active
 JOIN teams t ON t.team_id=b.team_id AND t.deleted_at IS NULL
 JOIN team_members member ON member.team_id=b.team_id AND member.account_id=actor AND member.active
 JOIN team_resource_grants g ON g.team_id=b.team_id AND g.account_id=actor AND g.resource_kind=e.kind AND g.resource_id=e.id AND g.active AND g.permission IN ('use','manage')
 WHERE team_paid_entitled(b.team_id) AND (e.owner_team_id IS NULL OR e.owner_team_id=b.team_id) AND (e.owner_team_id=b.team_id OR (m.owner_team_id IS NULL AND e.access_mode='team') OR m.owner_team_id=b.team_id)
 UNION ALL
 SELECT 'automatic',m.id,m.machine_service_policy_generation,NULL::text FROM host m
 WHERE port>=1024 AND m.owner_team_id IS NULL AND m.user_id=actor AND m.personal_auto_enabled
 UNION ALL
 SELECT 'automatic',m.id,m.machine_service_policy_generation,b.team_id FROM host m
 JOIN team_resource_bindings b ON b.resource_kind='machine' AND b.resource_id=m.id AND b.active
 WHERE team_machine_port_allowed(actor,b.team_id,m.id,port)
 ;
$$;
-- +goose StatementEnd

-- Who can see a machine: the third branch was "members with a grant on it"; it is now
-- "members the policy allows anything on it".
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_audience(wanted_machine text) RETURNS text[]
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(DISTINCT audience.id), '{}'::text[]) FROM (
  SELECT peer.id FROM paperboat.user_machines own
   JOIN paperboat.user_machines peer ON peer.user_id = own.user_id AND peer.deleted_at IS NULL
   WHERE own.id = wanted_machine
  UNION
  SELECT peer.id FROM paperboat.user_machines own
   JOIN paperboat.team_members member ON member.team_id = own.owner_team_id AND member.active
   JOIN paperboat.user_machines peer ON peer.user_id = member.account_id AND peer.deleted_at IS NULL
   WHERE own.id = wanted_machine AND own.owner_team_id IS NOT NULL
  UNION
  SELECT peer.id FROM paperboat.team_resource_bindings b
   JOIN paperboat.team_members member ON member.team_id = b.team_id AND member.active
   JOIN paperboat.user_machines peer ON peer.user_id = member.account_id AND peer.deleted_at IS NULL
   WHERE b.resource_kind = 'machine' AND b.resource_id = wanted_machine AND b.active
    AND EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) cap
      WHERE NOT isempty(paperboat.workspace_policy_ports(member.account_id, b.team_id, wanted_machine, cap)))
 ) audience
$$;
-- +goose StatementEnd

-- A saved policy can change access on every machine of the team: each of them refreshes its
-- access (the topic team_machine_grants_publish used to send to one machine).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_workspace_policy() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.team_id IS NOT NULL THEN
    PERFORM paperboat.publish_machine_topic(coalesce((SELECT array_agg(resource_id) FROM paperboat.team_resource_bindings
      WHERE team_id = NEW.team_id AND resource_kind = 'machine' AND active), '{}'::text[]), 'machine_access');
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER workspace_policies_publish
    AFTER INSERT OR UPDATE OF version ON paperboat.workspace_policies
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_workspace_policy();

-- Private-access sessions are fenced by the policy version where they were fenced by the grant
-- generation (the team generation, which a save also advances, stays in the fence).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_service_team_fence(actor text, machine text, kind text, source text, team text) RETURNS text
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT CASE WHEN team IS NULL THEN 'personal' ELSE (
 SELECT jsonb_build_array(t.generation,member.membership_generation,b.generation,
 CASE WHEN kind='automatic' THEN (SELECT p.version FROM workspace_policies p WHERE p.team_id=t.team_id)
 ELSE (SELECT g.generation FROM team_resource_grants g WHERE g.team_id=t.team_id AND g.account_id=actor AND g.resource_kind=kind AND g.resource_id=source AND g.active AND g.permission IN ('use','manage')) END)::text
 FROM teams t JOIN team_members member ON member.team_id=t.team_id AND member.account_id=actor AND member.active
 JOIN team_resource_bindings b ON b.team_id=t.team_id AND b.resource_kind=CASE WHEN kind='automatic' THEN 'machine' ELSE kind END AND b.resource_id=CASE WHEN kind='automatic' THEN machine ELSE source END AND b.active
 WHERE t.team_id=team AND t.deleted_at IS NULL
 ) END;
$$;
-- +goose StatementEnd

-- A saved policy revokes the team's active machine access sessions it no longer allows, as a
-- grant change did (revoke_invalid_team_machine_access reads NEW.team_id). The server writes
-- the rules before it advances the version, so the check sees the new rules.
CREATE TRIGGER workspace_policies_revoke_access
    AFTER UPDATE OF version ON paperboat.workspace_policies
    FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access();

-- team_machine_grants and teams.allow_private_access are no longer read or written. They are
-- dropped by a later migration, once the previous server can no longer be the rollback target
-- (a rolled-back server keeps running: it writes grants nobody reads and enforces the policy).

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'workspace policy migration is forward-only'; END $$;
-- +goose StatementEnd
