-- +goose Up
-- The revision an operator's node action (drain, drain-move, resume, retire) must name: each such action and a region
-- disable advance it by compare-and-set, so a request built on an older read cannot undo a newer one. Constant default:
-- a metadata-only change.
SET lock_timeout = '5s';
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN operator_revision bigint NOT NULL DEFAULT 1 CHECK (operator_revision > 0);

-- +goose Down
SET lock_timeout = '5s';
ALTER TABLE paperboat.control_tunnel_nodes DROP COLUMN operator_revision;
