-- +goose Up
-- contract-after: 197
-- Step 5 of the endpoint merger: tunnels and previews are served only on the agent's runtime carriers
-- (endpoint_edge_assignments), so the connector identity stack and the per-preview carrier attachment
-- path (tunnels' connectors, enrollments, credentials, rotation, sessions, route assignments;
-- previews' attachments and their outbox) and everything that read them are gone. The unit that ends
-- this is the code deploy that removed their readers; no row of these tables is served by anything now.

-- Views: the engine branch is all that is left.

CREATE OR REPLACE VIEW paperboat.preview_routes AS
 SELECT p.id AS preview_id,
    p.account_id,
    x.route_id AS operation_id,
    p.owner_machine_id,
    p.owner_session_id,
    p.owner_machine_id AS host_id,
    COALESCE((ra.admission -> 'binding'::text) ->> 'tunnel_id'::text, ''::text)::text AS tunnel_id,
    COALESCE((ra.admission -> 'binding'::text) ->> 'connector_id'::text, ''::text)::text AS connector_id,
    ra.session_id AS connector_session_id,
    ra.worker_generation AS process_generation,
    COALESCE(((ra.admission -> 'binding'::text) ->> 'config_generation'::text)::bigint, 0::bigint)::bigint AS config_generation,
    x.route_id,
    r.revision AS route_generation,
    1::bigint AS lease_generation,
    COALESCE(ra.admission ->> 'config_content_hash'::text, ''::text)::text AS config_content_hash,
    x.edge_node_id,
    x.edge_process_epoch,
    x.generation AS attachment_generation,
    ra.public_identity_key AS machine_identity_public_key,
    COALESCE(ARRAY( SELECT jsonb_array_elements_text(ra.admission -> 'edge_endpoints'::text) AS jsonb_array_elements_text), '{}'::text[])::text[] AS edge_endpoints,
    COALESCE((ra.admission -> 'binding'::text) ->> 'edge_carrier_server_spki_sha256'::text, ''::text)::text AS edge_carrier_server_spki_sha256,
    COALESCE((ra.admission -> 'binding'::text) ->> 'edge_carrier_server_certificate_chain_pem'::text, ''::text)::text AS edge_carrier_server_certificate_chain_pem,
        CASE
            WHEN p.ready_at IS NOT NULL THEN 'ready'::text
            ELSE 'edge_ready'::text
        END AS state,
    true AS edge_ready,
    p.ready_at IS NOT NULL AS origin_ready,
    ra.expires_at,
    true AS endpoint
   FROM paperboat.endpoint_edge_assignments x
     JOIN paperboat.endpoint_routes r ON r.id = x.route_id
     JOIN paperboat.endpoints e ON e.id = x.endpoint_id AND e.kind = 'preview'::text AND e.desired_state = 'active'::text
     JOIN paperboat.preview_leases p ON p.id = e.preview_lease_id
     JOIN paperboat.runtime_carrier_attachments ra ON ra.machine_id = e.agent_machine_id AND ra.edge_node_id = x.edge_node_id AND ra.edge_process_epoch = x.edge_process_epoch AND ra.session_id = x.carrier_session_id
     JOIN paperboat.control_tunnel_nodes n ON n.id = x.edge_node_id AND n.process_epoch = x.edge_process_epoch AND n.last_heartbeat_at > (now() - '00:00:30'::interval)
  WHERE x.desired = 'active'::text AND x.observed_ready AND x.observed_at > (now() - '00:02:00'::interval) AND ra.expires_at > now() AND (EXISTS ( SELECT 1
           FROM paperboat.user_machines m
          WHERE m.id = e.agent_machine_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL));

CREATE OR REPLACE VIEW paperboat.tunnel_route_ready_edges_v1 AS
 SELECT e.account_id,
    e.tunnel_id,
    er.tunnel_route_id AS route_id,
    ea.edge_node_id,
    ea.edge_process_epoch,
    ea.generation AS assignment_generation
   FROM paperboat.endpoint_edge_assignments ea
     JOIN paperboat.endpoint_routes er ON er.id = ea.route_id
     JOIN paperboat.endpoints e ON e.id = er.endpoint_id
  WHERE e.kind = 'tunnel'::text AND e.desired_state = 'active'::text AND ea.desired = 'active'::text AND ea.observed_ready;


-- Tunnel management decisions no longer weigh other hosts' connectors: a tunnel is served by the machine that created it.

DROP FUNCTION paperboat.manageable_tunnel_ids(text, boolean, text, text, text, text, text, timestamp with time zone, text, integer);
DROP FUNCTION paperboat.manageable_tunnel_ids(text, boolean, text, text, text, text, text);
DROP FUNCTION paperboat.tunnel_management_allowed(text, text);
DROP FUNCTION paperboat.workspace_tunnel_management_allowed(text, text, text);
DROP FUNCTION paperboat.tunnel_management_decision(text, text, text, text, text, boolean, text, text, text[], boolean, boolean, boolean, boolean);
DROP FUNCTION paperboat.workspace_tunnel_management_decision(text, text, text, text, text, text, text, text, text[], boolean, boolean);
DROP FUNCTION paperboat.tunnel_connectors_blocked(text, text, text);
DROP FUNCTION paperboat.tunnel_connector_blocks(text, text, text, text, text, text, text, text[], boolean, boolean);

-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_tunnel_management_decision(actor text, team text, tunnel_id text, tunnel_account text, tunnel_team text, access_mode text, host_user text, host_team text, host_capabilities text[], host_manage boolean)
 RETURNS boolean
 LANGUAGE sql
 STABLE
 SET search_path TO 'paperboat', 'pg_catalog'
AS $function$
 SELECT (CASE WHEN team IS NULL THEN tunnel_team IS NULL AND tunnel_account=actor AND host_team IS NULL AND host_user=actor
  ELSE (host_capabilities @> ARRAY['preview_launch']::text[] OR (tunnel_account=actor AND host_user=actor AND host_team IS NULL))
   AND (access_mode IN ('private','team') OR (tunnel_account=actor AND host_user=actor AND host_team IS NULL))
   AND (tunnel_team=team OR (tunnel_team IS NULL AND EXISTS(SELECT 1 FROM team_resource_bindings b WHERE b.team_id=team
    AND b.resource_kind='tunnel' AND b.resource_id=tunnel_id AND b.owner_account=tunnel_account AND b.active))) END)
  AND (team IS NULL OR coalesce(host_manage, false));
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.tunnel_management_decision(actor text, tunnel_id text, tunnel_account text, tunnel_team text, access_mode text, deleted boolean, host_user text, host_team text, host_capabilities text[], team_host_manage boolean, host_manage boolean)
 RETURNS boolean
 LANGUAGE sql
 STABLE
 SET search_path TO 'paperboat', 'pg_catalog'
AS $function$
 SELECT (tunnel_team IS NOT NULL AND workspace_tunnel_management_decision(actor, tunnel_team, tunnel_id, tunnel_account, tunnel_team,
   access_mode, host_user, host_team, host_capabilities, team_host_manage))
  OR (tunnel_team IS NULL AND ((tunnel_account=actor AND host_team IS NULL AND host_user=actor)
   OR (access_mode IN ('private','team') AND NOT deleted AND host_capabilities @> ARRAY['preview_launch']::text[]
    AND coalesce(host_manage, false))));
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.tunnel_management_allowed(actor text, tunnel text)
 RETURNS boolean
 LANGUAGE sql
 STABLE
 SET search_path TO 'paperboat', 'pg_catalog'
AS $function$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id WHERE t.id=tunnel
  AND tunnel_management_decision(actor, t.id, t.account_id, t.owner_team_id, t.access_mode, t.deleted_at IS NOT NULL, m.user_id, m.owner_team_id,
   m.configured_capabilities,
   CASE WHEN t.owner_team_id IS NOT NULL THEN workspace_publisher_host_capability_allowed(actor, t.owner_team_id, m.id, 'tunnel_manage') END,
   CASE WHEN t.owner_team_id IS NULL AND t.access_mode IN ('private','team') THEN machine_capability_allowed(actor, m.id, 'tunnel_manage') END));
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_tunnel_management_allowed(actor text, team text, tunnel text)
 RETURNS boolean
 LANGUAGE sql
 STABLE
 SET search_path TO 'paperboat', 'pg_catalog'
AS $function$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id WHERE t.id=tunnel
  AND workspace_tunnel_management_decision(actor, team, t.id, t.account_id, t.owner_team_id, t.access_mode, m.user_id, m.owner_team_id,
   m.configured_capabilities, CASE WHEN team IS NOT NULL THEN workspace_publisher_host_capability_allowed(actor, team, m.id, 'tunnel_manage') END));
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.manageable_tunnel_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text, machine text, after_created_at timestamp with time zone, after_id text, row_limit integer)
 RETURNS SETOF text
 LANGUAGE sql
 STABLE
 SET search_path TO 'paperboat', 'pg_catalog'
AS $function$
 WITH rules AS (
   -- The allow rules for tunnel_manage in the account's teams whose subject may be the account (any role: a superset).
   SELECT tm.team_id, r.policy_id, r.target_kind, r.target_id FROM team_members AS tm
   JOIN workspace_policies AS p ON p.team_id = tm.team_id
   JOIN workspace_policy_rules AS r ON r.policy_id = p.id AND r.action = 'tunnel_manage' AND NOT r.deny
   WHERE tm.account_id = actor AND tm.active
     AND (r.subject_kind IN ('all', 'role') OR (r.subject_kind = 'account' AND r.subject_id = actor)
       OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups AS g
         WHERE g.policy_id = r.policy_id AND g.group_name = r.subject_id AND g.account_id = actor)))
 ), targets AS (
   SELECT r.team_id, r.target_id AS machine_id FROM rules AS r WHERE r.target_kind = 'machine'
   UNION
   SELECT r.team_id, tg.machine_id FROM rules AS r
   JOIN workspace_policy_tags AS tg ON tg.policy_id = r.policy_id AND tg.tag = r.target_id WHERE r.target_kind = 'tag'
 ), hosts AS (
   SELECT m.id FROM user_machines AS m WHERE m.user_id = actor AND m.owner_team_id IS NULL
   UNION
   SELECT b.resource_id FROM team_resource_bindings AS b JOIN targets AS t ON t.team_id = b.team_id AND t.machine_id = b.resource_id
   WHERE b.resource_kind = 'machine' AND b.active
   UNION
   SELECT b.resource_id FROM team_resource_bindings AS b JOIN rules AS r ON r.team_id = b.team_id AND r.target_kind = 'all_machines'
   WHERE b.resource_kind = 'machine' AND b.active
 ), candidates AS MATERIALIZED (
  SELECT t.* FROM tunnels AS t
  WHERE t.created_by_host_id IN (SELECT hosts.id FROM hosts)
    AND (t.account_id = actor OR t.access_mode IN ('private', 'team'))
    AND (q='' OR position(lower(q) in lower(t.id||' '||t.name||' '||t.stable_endpoint||' '||t.created_by_host_id))>0)
    AND ((state='' AND t.desired_state<>'deleted') OR t.desired_state=state)
    AND (owner='' OR (owner='mine' AND t.account_id=actor) OR (owner='shared' AND t.account_id<>actor) OR t.account_id=owner)
    AND (machine='' OR t.created_by_host_id=machine)
    AND (after_created_at IS NULL OR (t.created_at, t.id) < (after_created_at, after_id))
 ), used AS MATERIALIZED (
  SELECT m.* FROM user_machines AS m WHERE m.id IN (SELECT t.created_by_host_id FROM candidates AS t)
 ), host_rights AS MATERIALIZED (
  SELECT m.id, machine_capability_allowed(actor, m.id, 'tunnel_manage') AS manage FROM used AS m
 ), team_rights AS MATERIALIZED (
  SELECT m.id, t.team_id, workspace_publisher_host_capability_allowed(actor, t.team_id, m.id, 'tunnel_manage') AS manage
  FROM used AS m CROSS JOIN (SELECT c.owner_team_id AS team_id FROM candidates AS c WHERE c.owner_team_id IS NOT NULL
   UNION SELECT workspace_team WHERE workspace_team IS NOT NULL) AS t
 )
 -- Newest first, decided one row at a time until the page is full (OFFSET 0 keeps the order below the filter);
 -- the permission checks run only for the rows that get that far.
 SELECT x.id FROM (
  SELECT t.id, t.account_id, t.owner_team_id, t.access_mode, t.deleted_at IS NOT NULL AS deleted, m.id AS host_id,
   m.user_id AS host_user, m.owner_team_id AS host_team, m.configured_capabilities AS host_capabilities,
   h.manage AS host_manage, own.manage AS own_team_manage, ws.manage AS ws_team_manage
  FROM candidates AS t JOIN used AS m ON m.id = t.created_by_host_id
   LEFT JOIN host_rights AS h ON h.id = m.id
   LEFT JOIN team_rights AS own ON own.id = m.id AND own.team_id = t.owner_team_id
   LEFT JOIN team_rights AS ws ON ws.id = m.id AND ws.team_id = workspace_team
  ORDER BY t.created_at DESC, t.id DESC OFFSET 0
 ) AS x
 WHERE tunnel_management_decision(actor, x.id, x.account_id, x.owner_team_id, x.access_mode, x.deleted, x.host_user,
   x.host_team, x.host_capabilities, x.own_team_manage, x.host_manage)
  AND (NOT workspace_set OR workspace_tunnel_management_decision(actor, workspace_team, x.id, x.account_id, x.owner_team_id,
   x.access_mode, x.host_user, x.host_team, x.host_capabilities, x.ws_team_manage))
 LIMIT row_limit;
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.manageable_tunnel_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text, machine text)
 RETURNS SETOF text
 LANGUAGE sql
 STABLE
 SET search_path TO 'paperboat', 'pg_catalog'
AS $function$
 SELECT manageable_tunnel_ids(actor, workspace_set, workspace_team, q, state, owner, machine, NULL, NULL, NULL);
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.tunnel_host_ids(wanted_tunnel text)
 RETURNS text[]
 LANGUAGE sql
 STABLE
AS $function$
 SELECT coalesce(array_agg(DISTINCT host_id), '{}'::text[]) FROM (
  SELECT created_by_host_id AS host_id FROM paperboat.tunnels WHERE id = wanted_tunnel
 ) hosts
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.project_edge_bandwidth(receipt_operation text)
 RETURNS void
 LANGUAGE sql
AS $function$
 DELETE FROM paperboat.resource_bandwidth_events WHERE operation_id IN (SELECT operation_id FROM paperboat.resource_bandwidth_events WHERE interval_end<now()-interval '90 days' LIMIT 500);
 INSERT INTO paperboat.resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,machine_id,machine_name,mode,hosting,node_id,node_name,upload_bytes,download_bytes,interval_start,interval_end,actor_account_id)
 SELECT 'edge:'||r.operation_id,
 coalesce(r.workspace_id,a.workspace_id,CASE WHEN p.id IS NOT NULL THEN coalesce('team:'||p.owner_team_id,'personal:'||p.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.account_id) END),
 CASE WHEN p.id IS NOT NULL THEN 'preview' WHEN t.id IS NOT NULL THEN 'tunnel' ELSE 'route' END,
 coalesce(p.id,t.id,r.route_id),coalesce(p.endpoint,t.name,r.route_id),coalesce(m.id,''),coalesce(m.alias,''),
 'edge',CASE WHEN i.scope_kind IN ('account','team') THEN 'self_hosted' WHEN n.id IS NOT NULL THEN 'managed' ELSE 'unknown' END,
 r.edge_node_id,coalesce(i.name,r.edge_node_id),CASE WHEN r.direction='ingress' THEN r.delta_bytes ELSE 0 END,CASE WHEN r.direction='egress' THEN r.delta_bytes ELSE 0 END,r.interval_start,r.interval_end,coalesce(r.actor_account_id,a.actor_account_id)
 FROM paperboat.control_usage_receipts r
 LEFT JOIN paperboat.usage_accounting_authorities a ON a.authority_id=r.authority_id AND a.edge_node_id=r.edge_node_id AND a.environment_id=r.environment_id AND a.route_id=r.route_id AND a.route_revision=r.route_revision
 LEFT JOIN LATERAL (SELECT e.preview_lease_id AS preview_id FROM paperboat.endpoint_routes er JOIN paperboat.endpoints e ON e.id=er.endpoint_id AND e.kind='preview' WHERE er.id=r.route_id AND er.revision=r.route_revision LIMIT 1) pa ON true
 LEFT JOIN paperboat.preview_leases p ON p.id=pa.preview_id
 LEFT JOIN paperboat.tunnel_routes tr ON tr.id=r.route_id
 LEFT JOIN paperboat.tunnels t ON t.id=tr.tunnel_id AND p.id IS NULL
 LEFT JOIN paperboat.user_machines m ON m.id=coalesce(p.owner_machine_id,t.created_by_host_id)
 LEFT JOIN paperboat.selfhost_installations i ON i.node_id=r.edge_node_id
 LEFT JOIN paperboat.control_tunnel_nodes n ON n.id=r.edge_node_id
 WHERE r.operation_id=receipt_operation AND r.delta_bytes>0
 AND coalesce(r.workspace_id,a.workspace_id,CASE WHEN p.id IS NOT NULL THEN coalesce('team:'||p.owner_team_id,'personal:'||p.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.account_id) END) IS NOT NULL
 ON CONFLICT(operation_id) DO NOTHING;
$function$;
-- +goose StatementEnd


-- The agent reports origin health per machine (POST /v1/runtime/endpoint-tunnels/origin-health): the
-- row is keyed by the reporting machine, not a connector. Old rows name connectors and age out anyway.
DELETE FROM paperboat.tunnel_route_origin_health;
ALTER TABLE paperboat.tunnel_route_origin_health DROP CONSTRAINT tunnel_route_origin_health_connector_id_fkey;
ALTER TABLE paperboat.tunnel_route_origin_health DROP COLUMN session_id;
ALTER TABLE paperboat.tunnel_route_origin_health RENAME COLUMN connector_id TO machine_id;
ALTER TABLE paperboat.tunnel_log_entries DROP CONSTRAINT tunnel_log_entries_connector_id_tunnel_id_fkey;

DROP TABLE paperboat.connector_proof_replays, paperboat.tunnel_connector_activations,
 paperboat.tunnel_connector_rotation_targets, paperboat.tunnel_connector_credential_generations,
 paperboat.tunnel_connector_enrollments, paperboat.tunnel_connector_sessions,
 paperboat.tunnel_edge_route_assignments, paperboat.tunnel_connectors,
 paperboat.preview_carrier_attachment_outbox, paperboat.preview_lease_carrier_attachments;
DROP FUNCTION paperboat.validate_connector_rotation_target_scope();
DROP FUNCTION paperboat.publish_tunnel_connector();
DROP FUNCTION paperboat.preview_lease_carrier_attachment_guard_v1();
DROP FUNCTION paperboat.endpoint_lease_attachment_changed();
DROP FUNCTION paperboat.preview_lease_carrier_attachment_edge_epoch_guard_v1();

-- +goose Down
-- Forward only: the connector tables and their data are not restored.
SELECT 1;
