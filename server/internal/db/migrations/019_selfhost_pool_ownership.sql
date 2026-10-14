-- +goose Up
-- +goose StatementBegin
-- Pools choose traffic for an account; teams own nodes made available to members.
DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='team';
ALTER TABLE paperboat.selfhost_pool_policies ADD CONSTRAINT selfhost_pool_account_scope CHECK(scope_kind='account');
CREATE OR REPLACE FUNCTION paperboat.node_pool_eligible(wanted_node text,wanted_account text,wanted_capability text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT wanted_capability IN ('relay','tunnel') AND EXISTS(
 SELECT 1 FROM paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.selfhost_installations i ON i.node_id=n.id
 LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id
 LEFT JOIN paperboat.operator_regions r ON r.id=m.region_id
 LEFT JOIN paperboat.selfhost_pool_policies personal ON personal.scope_kind='account' AND personal.scope_id=wanted_account AND personal.capability=wanted_capability
 WHERE n.id=wanted_node AND (
 ((i.id IS NULL OR(i.scope_kind='global' AND i.state='active' AND i.claim_complete AND octet_length(n.tls_spki_sha256)=32)) AND (m.node_id IS NULL OR(m.retired_at IS NULL AND r.enabled)) AND coalesce(personal.mode,'mixed')='mixed' AND(n.allowed_account_ids IS NULL OR wanted_account=ANY(n.allowed_account_ids)))
 OR(i.scope_kind<>'global' AND i.state='active' AND i.claim_complete AND octet_length(n.tls_spki_sha256)=32 AND i.capability=wanted_capability AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,wanted_account)
 AND(personal.scope_id IS NULL OR i.id=ANY(personal.installation_ids)))
 ));
$$;
-- +goose StatementEnd
