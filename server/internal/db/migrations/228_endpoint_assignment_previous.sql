-- +goose Up
-- The carrier session and generation an endpoint assignment had before a worker handover moved it, read with
-- runtime_carrier_attachments.previous_admission during the overlap.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.endpoint_edge_assignments ADD COLUMN previous_carrier_session_id text, ADD COLUMN previous_generation bigint;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.endpoint_edge_assignments DROP COLUMN previous_carrier_session_id, DROP COLUMN previous_generation;
