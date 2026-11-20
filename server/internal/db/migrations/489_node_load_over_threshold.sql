-- +goose Up
SET LOCAL lock_timeout = '5s';
-- The hysteresis state of the load rule, kept apart from out_for_load (what readers use): a region's least loaded node
-- is kept in rotation even above the out threshold, and that forcing must not become the state its next round starts from.
ALTER TABLE paperboat.node_load_out ADD COLUMN over_threshold boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE paperboat.node_load_out DROP COLUMN over_threshold;
