-- +goose Up
-- A native inspector admission for a preview read the legacy preview_lease_carrier_attachments, so a
-- preview served as an endpoint route (PLAN 3a) never got its inspector network scope and the
-- owner's `pb preview inspect` was refused at the native dial ("virtual UDP requires explicit peer
-- and port admission"). It reads preview_routes, like every other preview read path.
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
             JOIN paperboat.preview_routes x ON (((x.preview_id = p.id) AND (x.account_id = p.account_id))))
          WHERE ((p.id = i.resource_id) AND (p.account_id = i.owner_account_id) AND (p.terminal_state = 'active'::text) AND (p.lease_deadline > now()) AND ((p.user_deadline IS NULL) OR (p.user_deadline > now())) AND (x.state = 'ready'::text) AND (x.expires_at > now()) AND (x.host_id = i.native_machine_id) AND (x.route_generation = i.resource_generation) AND (x.route_generation = i.route_generation) AND (x.route_generation = i.target_generation))))) OR ((i.resource_kind = 'tunnel'::text) AND (EXISTS ( SELECT 1
           FROM (paperboat.tunnels t
             JOIN paperboat.tunnel_routes r ON ((r.tunnel_id = t.id)))
          WHERE ((t.id = i.resource_id) AND (t.account_id = i.owner_account_id) AND (t.generation = i.resource_generation) AND (t.desired_state = 'active'::text) AND (t.deleted_at IS NULL) AND ((t.expires_at IS NULL) OR (t.expires_at > now())) AND (r.id = i.route_id) AND (r.generation = i.route_generation) AND (r.generation = i.target_generation) AND (r.desired_state = 'active'::text) AND (r.deleted_at IS NULL) AND (t.created_by_host_id = i.native_machine_id)))))));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'inspector native admission migration is forward-only'; END $$;
-- +goose StatementEnd
