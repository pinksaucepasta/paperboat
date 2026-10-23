-- +goose Up
-- Preview and tunnel inspect/replay in the workspace policy (PLAN F18-02/F18-03, F17). The
-- inspect and replay grants (team_resource_grants kind preview/tunnel, permission inspect or
-- replay) are no longer read or written: each team's policy gets a new version with identical
-- access, one rule per member and resource ({"src": ["user:#<account>"], "dst":
-- ["preview:#<id>"], "allow": ["inspect", "replay"]}). use/manage grants stay grants.

-- Whether actor may do wanted (inspect or replay) on one preview or tunnel bound to team: its
-- owner, or the owner of the personal machine it runs on, always; others by an allow rule on
-- the resource or on the team machine it runs on (machine, every machine, tag), and no deny.
-- inspect does not imply replay. shared/policy Compiled.Decide is the same evaluation.
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_policy_resource_allowed(actor text, team text, kind text, resource text, wanted text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH p AS (SELECT id FROM workspace_policies WHERE team_id = team),
 actor_role AS (
  SELECT CASE WHEN t.owner_account = actor THEN 'owner' ELSE member.role END AS role
  FROM teams t JOIN team_members member USING (team_id)
  WHERE t.team_id = team AND member.account_id = actor AND member.active
 ), res AS (
  SELECT account_id AS owner, owner_machine_id AS machine FROM preview_leases WHERE kind = 'preview' AND id = resource
  UNION ALL
  SELECT account_id, created_by_host_id FROM tunnels WHERE kind = 'tunnel' AND id = resource
 ), mach AS (
  SELECT m.id, CASE WHEN m.owner_team_id IS NULL THEN m.user_id END AS personal
  FROM user_machines m JOIN res ON m.id = res.machine
  JOIN team_resource_bindings b ON b.team_id = team AND b.resource_kind = 'machine' AND b.resource_id = m.id AND b.active
  WHERE m.deleted_at IS NULL
 ), matching AS (
  SELECT r.deny FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id CROSS JOIN actor_role a
  WHERE r.action = wanted
   AND (r.subject_kind = 'all'
     OR (r.subject_kind = 'account' AND r.subject_id = actor)
     OR (r.subject_kind = 'role' AND (r.subject_id = 'member' OR r.subject_id = a.role OR (r.subject_id = 'admin' AND a.role = 'owner')))
     OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id = p.id AND g.group_name = r.subject_id AND g.account_id = actor)))
   AND ((r.target_kind = kind AND r.target_id = resource)
     OR EXISTS(SELECT 1 FROM mach WHERE r.target_kind = 'all_machines'
       OR (r.target_kind = 'machine' AND r.target_id = mach.id)
       OR (r.target_kind = 'tag' AND EXISTS(SELECT 1 FROM workspace_policy_tags tg WHERE tg.policy_id = p.id AND tg.tag = r.target_id AND tg.machine_id = mach.id))))
 )
 SELECT wanted IN ('inspect', 'replay') AND kind IN ('preview', 'tunnel') AND EXISTS(SELECT 1 FROM actor_role)
  AND (EXISTS(SELECT 1 FROM res WHERE owner = actor) OR EXISTS(SELECT 1 FROM mach WHERE personal = actor)
    OR (EXISTS(SELECT 1 FROM matching WHERE NOT deny) AND NOT EXISTS(SELECT 1 FROM matching WHERE deny)));
$$;
-- +goose StatementEnd

-- Today's inspect/replay grants as rules: active grants of active members on resources bound
-- to the team (others allow nothing today).
CREATE TEMPORARY TABLE policy_inspect_migration ON COMMIT DROP AS
SELECT g.team_id, g.account_id, g.resource_kind, g.resource_id, array_agg(DISTINCT g.permission ORDER BY g.permission) AS actions
FROM paperboat.team_resource_grants g
JOIN paperboat.team_members m ON m.team_id = g.team_id AND m.account_id = g.account_id AND m.active
JOIN paperboat.team_resource_bindings b ON b.team_id = g.team_id AND b.resource_kind = g.resource_kind AND b.resource_id = g.resource_id AND b.active
JOIN paperboat.teams t ON t.team_id = g.team_id AND t.deleted_at IS NULL
WHERE g.active AND g.resource_kind IN ('preview', 'tunnel') AND g.permission IN ('inspect', 'replay')
GROUP BY g.team_id, g.account_id, g.resource_kind, g.resource_id;

-- +goose StatementBegin
DO $$
DECLARE
  w record;
  doc jsonb;
  base integer;
  next_source text;
BEGIN
  FOR w IN SELECT p.id, p.team_id, p.version, p.source FROM paperboat.workspace_policies p
           WHERE EXISTS(SELECT 1 FROM policy_inspect_migration e WHERE e.team_id = p.team_id) ORDER BY p.id LOOP
    BEGIN
      doc := w.source::jsonb;
    EXCEPTION WHEN others THEN
      RAISE EXCEPTION 'team % policy version % is not plain JSON; add its inspect/replay rules by hand, then rerun', w.team_id, w.version;
    END;
    base := coalesce(jsonb_array_length(doc -> 'rules'), 0);
    doc := jsonb_set(doc, '{rules}', coalesce(doc -> 'rules', '[]'::jsonb) || (
      SELECT jsonb_agg(jsonb_build_object('src', jsonb_build_array('user:#' || e.account_id),
               'dst', jsonb_build_array(e.resource_kind || ':#' || e.resource_id), 'allow', to_jsonb(e.actions))
             ORDER BY e.account_id, e.resource_kind, e.resource_id)
      FROM policy_inspect_migration e WHERE e.team_id = w.team_id));
    next_source := jsonb_pretty(doc);
    INSERT INTO paperboat.workspace_policy_rules(policy_id, rule, deny, subject_kind, subject_id, target_kind, target_id, action, ports)
    SELECT w.id, base + e.n - 1, false, 'account', e.account_id, e.resource_kind, e.resource_id, a, int4range(1, 65536)
    FROM (SELECT *, (row_number() OVER (ORDER BY account_id, resource_kind, resource_id))::integer AS n
          FROM policy_inspect_migration WHERE team_id = w.team_id) e
    CROSS JOIN LATERAL unnest(e.actions) a;
    UPDATE paperboat.workspace_policies SET version = w.version + 1, source = next_source,
      digest = 'sha256:' || encode(sha256(convert_to(next_source, 'UTF8')), 'hex'),
      message = 'Added preview and tunnel inspect/replay access from the grants', updated_at = now(), updated_by = NULL, updated_with_api_token = NULL
    WHERE id = w.id;
    INSERT INTO paperboat.workspace_policy_versions(policy_id, version, kind, source, digest, message)
    SELECT id, version, 'migrated', source, digest, message FROM paperboat.workspace_policies WHERE id = w.id;
  END LOOP;
END $$;
-- +goose StatementEnd

-- A native inspector admission on the team path holds while the policy still allows it and
-- has not changed since the credential was issued (grant_generation is the policy version).
CREATE OR REPLACE VIEW paperboat.inspector_native_admissions AS
 SELECT i.credential_id,
    i.account_id,
    i.owner_account_id,
    i.native_cli_session_id,
    i.native_machine_id,
    i.expires_at
   FROM ((((paperboat.inspector_credentials i
     JOIN paperboat.cli_client_sessions c ON (((c.id = i.native_cli_session_id) AND (c.user_id = i.account_id) AND (c.state = 'active'::text) AND (c.revoked_at IS NULL))))
     JOIN paperboat.user_machines m ON (((m.id = i.native_machine_id) AND (m.user_id = i.owner_account_id) AND (m.installation_generation = i.native_machine_generation) AND (m.deleted_at IS NULL) AND (m.revoked_at IS NULL))))
     JOIN paperboat.users actor ON (((actor.id = i.account_id) AND (actor.status = 'active'::text))))
     JOIN paperboat.users owner ON (((owner.id = i.owner_account_id) AND (owner.status = 'active'::text))))
  WHERE ((i.revoked_at IS NULL) AND (((i.team_id IS NULL) AND (i.account_id = i.owner_account_id)) OR (EXISTS ( SELECT 1
           FROM (((paperboat.teams t
             JOIN paperboat.team_members tm ON (((tm.team_id = t.team_id) AND (tm.account_id = i.account_id))))
             JOIN paperboat.team_resource_bindings b ON (((b.team_id = t.team_id) AND (b.resource_kind = i.resource_kind) AND (b.resource_id = i.resource_id))))
             JOIN paperboat.workspace_policies w ON ((w.team_id = t.team_id)))
          WHERE ((t.team_id = i.team_id) AND (t.deleted_at IS NULL) AND (t.generation = i.team_generation) AND tm.active AND (tm.membership_generation = i.membership_generation) AND b.active AND (b.owner_account = i.owner_account_id) AND (b.generation = i.binding_generation) AND (w.version = i.grant_generation) AND paperboat.workspace_policy_resource_allowed(i.account_id, t.team_id, i.resource_kind, i.resource_id, i.action))))) AND (((i.resource_kind = 'preview'::text) AND (EXISTS ( SELECT 1
           FROM (paperboat.preview_leases p
             JOIN paperboat.preview_lease_carrier_attachments x ON (((x.preview_id = p.id) AND (x.account_id = p.account_id))))
          WHERE ((p.id = i.resource_id) AND (p.account_id = i.owner_account_id) AND (p.terminal_state = 'active'::text) AND (p.lease_deadline > now()) AND ((p.user_deadline IS NULL) OR (p.user_deadline > now())) AND (x.state = 'ready'::text) AND (x.expires_at > now()) AND (x.host_id = i.native_machine_id) AND (x.route_generation = i.resource_generation) AND (x.route_generation = i.route_generation) AND (x.route_generation = i.target_generation))))) OR ((i.resource_kind = 'tunnel'::text) AND (EXISTS ( SELECT 1
           FROM (paperboat.tunnels t
             JOIN paperboat.tunnel_routes r ON ((r.tunnel_id = t.id)))
          WHERE ((t.id = i.resource_id) AND (t.account_id = i.owner_account_id) AND (t.generation = i.resource_generation) AND (t.desired_state = 'active'::text) AND (t.deleted_at IS NULL) AND ((t.expires_at IS NULL) OR (t.expires_at > now())) AND (r.id = i.route_id) AND (r.generation = i.route_generation) AND (r.generation = i.target_generation) AND (r.desired_state = 'active'::text) AND (r.deleted_at IS NULL) AND (t.created_by_host_id = i.native_machine_id)))))));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'inspect/replay policy migration is forward-only'; END $$;
-- +goose StatementEnd
