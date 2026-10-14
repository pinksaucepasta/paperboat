-- +goose Up
-- +goose StatementBegin
-- The unreleased operator/pairing model is replaced, not maintained in parallel.
UPDATE paperboat.control_tunnel_nodes SET state='retired',ready=false,drain_deadline=now(),registry_expires_at=now(),node_generation=node_generation+1 WHERE id IN (SELECT node_id FROM paperboat.selfhost_installations);
DROP TABLE paperboat.selfhost_operator_nonces;
DROP TABLE paperboat.selfhost_pairing_codes;
DROP TABLE paperboat.selfhost_pairings;
DROP TABLE paperboat.selfhost_challenges;
DELETE FROM paperboat.selfhost_installations;
ALTER TABLE paperboat.selfhost_installations RENAME COLUMN operator_public_key TO installation_public_key;
ALTER TABLE paperboat.selfhost_installations DROP CONSTRAINT selfhost_installations_operator_public_key_key;
ALTER TABLE paperboat.selfhost_installations ADD COLUMN scope_kind text NOT NULL CHECK(scope_kind IN ('account','team','global'));
ALTER TABLE paperboat.selfhost_installations ADD COLUMN scope_id text NOT NULL;
ALTER TABLE paperboat.selfhost_installations ADD COLUMN claim_id text NOT NULL;
ALTER TABLE paperboat.selfhost_installations ADD COLUMN claim_complete boolean NOT NULL DEFAULT false;
ALTER TABLE paperboat.selfhost_installations ADD UNIQUE(installation_public_key,capability);
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN tls_spki_sha256 bytea CHECK(tls_spki_sha256 IS NULL OR octet_length(tls_spki_sha256)=32);
DROP TABLE paperboat.selfhost_pool_policies;
CREATE TABLE paperboat.selfhost_pool_policies(scope_kind text NOT NULL CHECK(scope_kind IN ('account','team')),scope_id text NOT NULL,capability text NOT NULL CHECK(capability IN ('relay','tunnel')),mode text NOT NULL CHECK(mode IN ('mixed','self-hosted-only')),installation_ids text[] NOT NULL DEFAULT '{}' CHECK(cardinality(installation_ids)<=32),PRIMARY KEY(scope_kind,scope_id,capability));
CREATE TABLE paperboat.selfhost_placements(resource_kind text NOT NULL CHECK(resource_kind IN ('preview','tunnel')),resource_id text NOT NULL,installation_id text NOT NULL REFERENCES paperboat.selfhost_installations(id),PRIMARY KEY(resource_kind,resource_id));
CREATE OR REPLACE FUNCTION paperboat.selfhost_scope_visible(kind text,id text,actor text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT kind='global' OR (kind='account' AND id=actor) OR (kind='team' AND EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members m USING(team_id) WHERE t.team_id=id AND t.deleted_at IS NULL AND m.account_id=actor AND m.active));
$$;
CREATE OR REPLACE FUNCTION paperboat.node_pool_eligible(wanted_node text,wanted_account text,wanted_capability text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT wanted_capability IN ('relay','tunnel') AND EXISTS(
 SELECT 1 FROM paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.selfhost_installations i ON i.node_id=n.id
 LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id
 LEFT JOIN paperboat.operator_regions r ON r.id=m.region_id
 LEFT JOIN paperboat.selfhost_pool_policies personal ON personal.scope_kind='account' AND personal.scope_id=wanted_account AND personal.capability=wanted_capability
 LEFT JOIN paperboat.selfhost_pool_policies scoped ON scoped.scope_kind=i.scope_kind AND scoped.scope_id=i.scope_id AND scoped.capability=wanted_capability
 WHERE n.id=wanted_node AND (
 (i.id IS NULL AND (m.node_id IS NULL OR(m.retired_at IS NULL AND r.enabled)) AND coalesce(personal.mode,'mixed')='mixed' AND(n.allowed_account_ids IS NULL OR wanted_account=ANY(n.allowed_account_ids)))
 OR(i.state='active' AND i.claim_complete AND i.capability=wanted_capability AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,wanted_account)
 AND(personal.scope_id IS NULL OR i.id=ANY(personal.installation_ids)) AND(scoped.scope_id IS NULL OR i.id=ANY(scoped.installation_ids)))
 ));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_scope_eligible(wanted_node text,wanted_account text,wanted_capability text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes n JOIN paperboat.selfhost_installations i ON i.node_id=n.id WHERE n.id=wanted_node AND i.capability=wanted_capability AND i.state='active' AND i.claim_complete AND octet_length(n.tls_spki_sha256)=32 AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,wanted_account));
$$;
CREATE OR REPLACE FUNCTION paperboat.node_preview_eligible(wanted_node text,wanted_account text,wanted_preview text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT (
 (NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind='preview' AND resource_id=wanted_preview) AND paperboat.node_pool_eligible(wanted_node,wanted_account,'tunnel'))
 OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements p JOIN paperboat.selfhost_installations i ON i.id=p.installation_id WHERE p.resource_kind='preview' AND p.resource_id=wanted_preview AND i.node_id=wanted_node AND paperboat.node_scope_eligible(wanted_node,wanted_account,'tunnel'))
 ) AND(NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations WHERE node_id=wanted_node AND scope_kind<>'global') OR EXISTS(SELECT 1 FROM paperboat.preview_domains WHERE preview_id=wanted_preview AND account_id=wanted_account AND deleted_at IS NULL AND ownership_state='verified' AND conflict_state='clear'));
$$;
CREATE OR REPLACE FUNCTION paperboat.node_route_eligible(wanted_node text,wanted_account text,wanted_route text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.tunnel_routes r WHERE r.id=wanted_route AND (
 (NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind='tunnel' AND resource_id=r.tunnel_id) AND paperboat.node_pool_eligible(wanted_node,wanted_account,'tunnel'))
 OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements p JOIN paperboat.selfhost_installations i ON i.id=p.installation_id WHERE p.resource_kind='tunnel' AND p.resource_id=r.tunnel_id AND i.node_id=wanted_node AND paperboat.node_scope_eligible(wanted_node,wanted_account,'tunnel'))
 ) AND(NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations WHERE node_id=wanted_node AND scope_kind<>'global') OR r.protocol<>'http' OR EXISTS(SELECT 1 FROM paperboat.tunnel_domains WHERE route_id=wanted_route AND account_id=wanted_account AND deleted_at IS NULL AND ownership_state='verified' AND conflict_state='clear')));
$$;
-- +goose StatementEnd
