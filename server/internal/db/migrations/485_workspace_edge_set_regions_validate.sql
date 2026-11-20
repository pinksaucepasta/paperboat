-- +goose Up
-- Validation scans the table without blocking its writers (SHARE UPDATE EXCLUSIVE); the lock timeout keeps a busy
-- table from queueing the migration behind writers.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.workspace_edge_sets VALIDATE CONSTRAINT workspace_edge_sets_regions_size;

-- +goose StatementBegin
-- A node may serve a workspace when the workspace has no pair yet (or one without regions) or the node's region is
-- one of the pair's regions.
CREATE OR REPLACE FUNCTION paperboat.workspace_edge_set_allows(wanted_node text, wanted_account text, wanted_team text) RETURNS boolean
    LANGUAGE sql STABLE AS $$
 SELECT NOT EXISTS (SELECT 1 FROM paperboat.workspace_edge_sets s
    WHERE s.scope_kind = CASE WHEN wanted_team IS NULL THEN 'account' ELSE 'team' END
      AND s.scope_id = coalesce(wanted_team, wanted_account) AND cardinality(s.regions) > 0)
  OR EXISTS (SELECT 1 FROM paperboat.workspace_edge_sets s JOIN paperboat.control_tunnel_nodes n ON n.id = wanted_node
    WHERE s.scope_kind = CASE WHEN wanted_team IS NULL THEN 'account' ELSE 'team' END
      AND s.scope_id = coalesce(wanted_team, wanted_account) AND n.region = ANY (s.regions));
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
