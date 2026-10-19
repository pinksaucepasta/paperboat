-- +goose Up
-- Each workspace (a personal account or a team) is served by one set of edges: either the
-- Paperboat-managed pool or the workspace's own selected servers, never a mix. The set is stored
-- so every placement consults the same answer; a workspace without a row is unrestricted until
-- the control plane assigns one.
CREATE TABLE paperboat.workspace_edge_sets (
    scope_kind text NOT NULL CHECK (scope_kind IN ('account', 'team')),
    scope_id text NOT NULL CHECK (length(scope_id) BETWEEN 1 AND 128),
    mode text NOT NULL CHECK (mode IN ('managed-only', 'self-hosted-only')),
    node_ids text[] NOT NULL CHECK (cardinality(node_ids) BETWEEN 1 AND 8),
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    assigned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_kind, scope_id)
);

-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_edge_set_allows(wanted_node text, wanted_account text, wanted_team text) RETURNS boolean
    LANGUAGE sql STABLE AS $$
 SELECT NOT EXISTS (SELECT 1 FROM paperboat.workspace_edge_sets s
    WHERE s.scope_kind = CASE WHEN wanted_team IS NULL THEN 'account' ELSE 'team' END
      AND s.scope_id = coalesce(wanted_team, wanted_account))
  OR EXISTS (SELECT 1 FROM paperboat.workspace_edge_sets s
    WHERE s.scope_kind = CASE WHEN wanted_team IS NULL THEN 'account' ELSE 'team' END
      AND s.scope_id = coalesce(wanted_team, wanted_account)
      AND wanted_node = ANY (s.node_ids));
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION paperboat.workspace_edge_set_allows(text, text, text);
DROP TABLE paperboat.workspace_edge_sets;
