-- +goose Up
-- serves_managed_wildcard gates where NEW managed placement may go (runtime attach, edge
-- candidates, tunnel assignment), not whether an attachment that already exists on an
-- edge outside the set is still valid: that would strand its machine until it reattaches.
-- Restore the eligibility function without the flag.
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
