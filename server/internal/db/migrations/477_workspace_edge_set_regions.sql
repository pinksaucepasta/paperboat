-- +goose Up
-- A workspace's edge pair is two REGIONS (a primary and a backup), not node ids: which machine of a region serves a
-- workspace is the server's choice, so replacing a machine never changes the pair. node_ids stays (nullable, no longer
-- read or written) for one deploy because the previous server still reads it; the next deploy drops it. An empty
-- regions array means "no restriction yet": the edge-set worker fills it from the pair node_ids named (migration 478)
-- or from the usable regions.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.workspace_edge_sets
    ADD COLUMN regions text[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT workspace_edge_sets_regions_size CHECK (cardinality(regions) <= 2) NOT VALID,
    ALTER COLUMN node_ids DROP NOT NULL,
    DROP CONSTRAINT workspace_edge_sets_node_ids_check;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.workspace_edge_sets
    DROP CONSTRAINT workspace_edge_sets_regions_size,
    DROP COLUMN regions;
