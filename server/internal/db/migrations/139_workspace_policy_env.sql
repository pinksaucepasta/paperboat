-- +goose Up
-- Team ENV in the workspace policy (PLAN F18-07). env_read/env_write rules on env:team decide
-- who may use the team ENV; the ENV grants (team_resource_grants kind env) are no longer read
-- or written. Each team's policy gets a new version with identical access: owners and admins
-- read and write (role:admin; the owner also by the owner guardrail), and every other active
-- member keeps exactly the ENV grant they have (read, or read and write). Version 1 of every
-- team policy is JSON written by migration 128 or a JSON save; a file that is not JSON stops
-- the migration with its team named, so nothing changes silently.

-- Whether actor may do wanted (env_read or env_write) on team's ENV: the owner always; others
-- by an allow rule on env:team and no deny rule. shared/policy Compiled.Decide is the same.
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_policy_env_allowed(actor text, team text, wanted text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH p AS (SELECT id FROM workspace_policies WHERE team_id = team),
 actor_role AS (
  SELECT CASE WHEN t.owner_account = actor THEN 'owner' ELSE member.role END AS role
  FROM teams t JOIN team_members member USING (team_id)
  WHERE t.team_id = team AND member.account_id = actor AND member.active
 ), matching AS (
  SELECT r.deny FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id CROSS JOIN actor_role a
  WHERE r.action = wanted AND r.target_kind = 'env' AND r.target_id = team
   AND (r.subject_kind = 'all'
     OR (r.subject_kind = 'account' AND r.subject_id = actor)
     OR (r.subject_kind = 'role' AND (r.subject_id = 'member' OR r.subject_id = a.role OR (r.subject_id = 'admin' AND a.role = 'owner')))
     OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id = p.id AND g.group_name = r.subject_id AND g.account_id = actor)))
 )
 SELECT EXISTS(SELECT 1 FROM actor_role WHERE role = 'owner')
  OR (EXISTS(SELECT 1 FROM matching WHERE NOT deny) AND NOT EXISTS(SELECT 1 FROM matching WHERE deny));
$$;
-- +goose StatementEnd

-- The team ENV permission the vault uses: write, read or '' (none).
-- +goose StatementBegin
CREATE FUNCTION paperboat.team_env_permission(actor text, team text) RETURNS text
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT CASE WHEN workspace_policy_env_allowed(actor, team, 'env_write') THEN 'write'
             WHEN workspace_policy_env_allowed(actor, team, 'env_read') THEN 'read' ELSE '' END;
$$;
-- +goose StatementEnd

-- Today's ENV access as rules: role:admin, then one rule per non-admin member with an ENV grant.
CREATE TEMPORARY TABLE policy_env_migration ON COMMIT DROP AS
SELECT t.team_id, g.account_id, bool_or(g.permission = 'write') AS write
FROM paperboat.teams t
JOIN paperboat.team_members m ON m.team_id = t.team_id AND m.active AND m.role <> 'admin' AND m.account_id <> t.owner_account
JOIN paperboat.team_resource_grants g ON g.team_id = t.team_id AND g.account_id = m.account_id
 AND g.resource_kind = 'env' AND g.resource_id = t.team_id AND g.active
WHERE t.deleted_at IS NULL
GROUP BY t.team_id, g.account_id;

-- A team created after migration 128 that nobody has edited yet has no policy: create it empty
-- first, so every team gets the same new version below.
INSERT INTO paperboat.workspace_policies(team_id, version, source, digest, message)
SELECT t.team_id, 1, '{"rules": []}', 'sha256:' || encode(sha256(convert_to('{"rules": []}', 'UTF8')), 'hex'), ''
FROM paperboat.teams t
WHERE t.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM paperboat.workspace_policies w WHERE w.team_id = t.team_id);
INSERT INTO paperboat.workspace_policy_versions(policy_id, version, kind, source, digest, message)
SELECT w.id, 1, 'migrated', w.source, w.digest, w.message FROM paperboat.workspace_policies w
WHERE w.team_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM paperboat.workspace_policy_versions v WHERE v.policy_id = w.id);

-- +goose StatementBegin
DO $$
DECLARE
  w record;
  doc jsonb;
  added jsonb;
  base integer;
  next_source text;
BEGIN
  FOR w IN SELECT p.id, p.team_id, p.version, p.source FROM paperboat.workspace_policies p WHERE p.team_id IS NOT NULL ORDER BY p.id LOOP
    BEGIN
      doc := w.source::jsonb;
    EXCEPTION WHEN others THEN
      RAISE EXCEPTION 'team % policy version % is not plain JSON; add its team ENV rules by hand, then rerun', w.team_id, w.version;
    END;
    base := coalesce(jsonb_array_length(doc -> 'rules'), 0);
    added := jsonb_build_array(jsonb_build_object('src', jsonb_build_array('role:admin'), 'dst', jsonb_build_array('env:team'), 'allow', jsonb_build_array('env_read', 'env_write')))
      || coalesce((SELECT jsonb_agg(jsonb_build_object('src', jsonb_build_array('user:#' || e.account_id), 'dst', jsonb_build_array('env:team'),
           'allow', CASE WHEN e.write THEN jsonb_build_array('env_read', 'env_write') ELSE jsonb_build_array('env_read') END) ORDER BY e.account_id)
         FROM policy_env_migration e WHERE e.team_id = w.team_id), '[]'::jsonb);
    doc := jsonb_set(doc, '{rules}', coalesce(doc -> 'rules', '[]'::jsonb) || added);
    next_source := jsonb_pretty(doc);
    INSERT INTO paperboat.workspace_policy_rules(policy_id, rule, deny, subject_kind, subject_id, target_kind, target_id, action, ports)
    SELECT w.id, base, false, 'role', 'admin', 'env', w.team_id, a, int4range(1, 65536) FROM unnest(ARRAY['env_read', 'env_write']) a;
    INSERT INTO paperboat.workspace_policy_rules(policy_id, rule, deny, subject_kind, subject_id, target_kind, target_id, action, ports)
    SELECT w.id, base + 1 + (row_number() OVER (ORDER BY e.account_id))::integer - 1, false, 'account', e.account_id, 'env', w.team_id, a.action, int4range(1, 65536)
    FROM policy_env_migration e
    CROSS JOIN LATERAL unnest(CASE WHEN e.write THEN ARRAY['env_read', 'env_write'] ELSE ARRAY['env_read'] END) a(action)
    WHERE e.team_id = w.team_id;
    UPDATE paperboat.workspace_policies SET version = w.version + 1, source = next_source,
      digest = 'sha256:' || encode(sha256(convert_to(next_source, 'UTF8')), 'hex'),
      message = 'Added team ENV access from the ENV grants', updated_at = now(), updated_by = NULL, updated_with_api_token = NULL
    WHERE id = w.id;
    INSERT INTO paperboat.workspace_policy_versions(policy_id, version, kind, source, digest, message)
    SELECT id, version, 'migrated', source, digest, message FROM paperboat.workspace_policies WHERE id = w.id;
  END LOOP;
END $$;
-- +goose StatementEnd

-- ENV grants (team_resource_grants kind env) are no longer read or written; they are dropped
-- with team_machine_grants once the previous server can no longer be the rollback target.

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'team ENV policy migration is forward-only'; END $$;
-- +goose StatementEnd
