-- +goose Up
-- A worker handover (pb update) replaces a machine's carrier row on each edge. The replaced carrier's admission
-- stays here for a bounded overlap so the edge's ingress authority keeps authorizing it until the new carrier
-- serves (make-before-break).
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.runtime_carrier_attachments ADD COLUMN previous_admission jsonb, ADD COLUMN previous_until timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.runtime_carrier_attachments DROP COLUMN previous_admission, DROP COLUMN previous_until;
