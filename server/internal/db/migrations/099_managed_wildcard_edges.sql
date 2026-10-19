-- +goose Up
-- Managed preview, tunnel and runtime hostnames resolve through a wildcard record, so a new name
-- needs no DNS work. serves_managed_wildcard marks the edges that wildcard reaches; only those
-- may carry a managed name. Edges default to included (no placement change on upgrade); an
-- operator clears the flag on an edge that the wildcard record does not reach.
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN serves_managed_wildcard boolean NOT NULL DEFAULT true;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_workspace_edge_eligible(wanted_node text, wanted_account text, wanted_team text)
 RETURNS boolean
 LANGUAGE sql
 STABLE
AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id
 LEFT JOIN paperboat.operator_regions region ON region.id=m.region_id
 WHERE n.id=wanted_node AND (wanted_team IS NULL OR EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members member USING(team_id) WHERE t.team_id=wanted_team AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=wanted_account AND member.active)) AND (
 (paperboat.workspace_edge_mode(wanted_account,wanted_team)='managed-only'
 AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=n.id AND i.scope_kind<>'global')
 AND n.serves_managed_wildcard
 AND (m.node_id IS NULL OR(m.retired_at IS NULL AND region.enabled))
 AND(n.allowed_account_ids IS NULL OR wanted_account=ANY(n.allowed_account_ids))) OR
 (paperboat.workspace_edge_mode(wanted_account,wanted_team)='self-hosted-only' AND EXISTS(
 SELECT 1 FROM paperboat.selfhost_installations i JOIN paperboat.selfhost_pool_policies p ON i.id=ANY(p.installation_ids)
 WHERE i.node_id=n.id AND i.capability='tunnel' AND i.state='active' AND i.claim_complete
 AND i.scope_kind<>'global' AND octet_length(n.tls_spki_sha256)=32
 AND p.capability='tunnel' AND p.mode='self-hosted-only'
 AND p.scope_kind=CASE WHEN wanted_team IS NULL THEN 'account' ELSE 'team' END AND p.scope_id=coalesce(wanted_team,wanted_account)
 AND ((wanted_team IS NULL AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,wanted_account)) OR(i.scope_kind='team' AND i.scope_id=wanted_team))
 ))));
$$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_workspace_edge_eligible(wanted_node text, wanted_account text, wanted_team text)
 RETURNS boolean
 LANGUAGE sql
 STABLE
AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id
 LEFT JOIN paperboat.operator_regions region ON region.id=m.region_id
 WHERE n.id=wanted_node AND (wanted_team IS NULL OR EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members member USING(team_id) WHERE t.team_id=wanted_team AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=wanted_account AND member.active)) AND (
 (paperboat.workspace_edge_mode(wanted_account,wanted_team)='managed-only'
 AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=n.id AND i.scope_kind<>'global')
 AND (m.node_id IS NULL OR(m.retired_at IS NULL AND region.enabled))
 AND(n.allowed_account_ids IS NULL OR wanted_account=ANY(n.allowed_account_ids))) OR
 (paperboat.workspace_edge_mode(wanted_account,wanted_team)='self-hosted-only' AND EXISTS(
 SELECT 1 FROM paperboat.selfhost_installations i JOIN paperboat.selfhost_pool_policies p ON i.id=ANY(p.installation_ids)
 WHERE i.node_id=n.id AND i.capability='tunnel' AND i.state='active' AND i.claim_complete
 AND i.scope_kind<>'global' AND octet_length(n.tls_spki_sha256)=32
 AND p.capability='tunnel' AND p.mode='self-hosted-only'
 AND p.scope_kind=CASE WHEN wanted_team IS NULL THEN 'account' ELSE 'team' END AND p.scope_id=coalesce(wanted_team,wanted_account)
 AND ((wanted_team IS NULL AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,wanted_account)) OR(i.scope_kind='team' AND i.scope_id=wanted_team))
 ))));
$$;
-- +goose StatementEnd
ALTER TABLE paperboat.control_tunnel_nodes DROP COLUMN serves_managed_wildcard;
