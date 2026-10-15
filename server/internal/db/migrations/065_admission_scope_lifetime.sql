-- +goose Up
-- Credential TTL gates admission, not existing session revocation authority.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_service_session_allowed(session text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS(SELECT 1 FROM user_machine_access_sessions a
 JOIN cli_client_sessions c ON c.id=a.cli_client_session_id AND c.user_id=a.user_id AND c.state='active' AND c.revoked_at IS NULL
 CROSS JOIN LATERAL machine_service_authorities(a.user_id,a.user_machine_id,a.machine_service_port) authority
 WHERE a.id=session AND a.state='active' AND a.revoked_at IS NULL
 AND authority.source_kind=a.machine_service_source_kind AND authority.source_id=a.machine_service_source_id
 AND authority.source_generation=a.machine_service_source_generation AND authority.team_id IS NOT DISTINCT FROM a.team_id
 AND a.machine_service_team_fence=machine_service_team_fence(a.user_id,a.user_machine_id,a.machine_service_source_kind,a.machine_service_source_id,a.team_id));
$$;
-- +goose StatementEnd

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
             JOIN paperboat.team_resource_grants g ON (((g.team_id = t.team_id) AND (g.account_id = i.account_id) AND (g.resource_kind = i.resource_kind) AND (g.resource_id = i.resource_id) AND (g.permission = i.action))))
          WHERE ((t.team_id = i.team_id) AND (t.deleted_at IS NULL) AND (t.generation = i.team_generation) AND tm.active AND (tm.membership_generation = i.membership_generation) AND b.active AND (b.owner_account = i.owner_account_id) AND (b.generation = i.binding_generation) AND g.active AND (g.generation = i.grant_generation))))) AND (((i.resource_kind = 'preview'::text) AND (EXISTS ( SELECT 1
           FROM (paperboat.preview_leases p
             JOIN paperboat.preview_lease_carrier_attachments x ON (((x.preview_id = p.id) AND (x.account_id = p.account_id))))
          WHERE ((p.id = i.resource_id) AND (p.account_id = i.owner_account_id) AND (p.terminal_state = 'active'::text) AND (p.lease_deadline > now()) AND ((p.user_deadline IS NULL) OR (p.user_deadline > now())) AND (x.state = 'ready'::text) AND (x.expires_at > now()) AND (x.host_id = i.native_machine_id) AND (x.route_generation = i.resource_generation) AND (x.route_generation = i.route_generation) AND (x.route_generation = i.target_generation))))) OR ((i.resource_kind = 'tunnel'::text) AND (EXISTS ( SELECT 1
           FROM (paperboat.tunnels t
             JOIN paperboat.tunnel_routes r ON ((r.tunnel_id = t.id)))
          WHERE ((t.id = i.resource_id) AND (t.account_id = i.owner_account_id) AND (t.generation = i.resource_generation) AND (t.desired_state = 'active'::text) AND (t.deleted_at IS NULL) AND ((t.expires_at IS NULL) OR (t.expires_at > now())) AND (r.id = i.route_id) AND (r.generation = i.route_generation) AND (r.generation = i.target_generation) AND (r.desired_state = 'active'::text) AND (r.deleted_at IS NULL) AND (t.created_by_host_id = i.native_machine_id)))))));

-- +goose Down
-- The admission-only lifetime decision is not rolled back on a live database.
