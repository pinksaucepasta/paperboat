-- +goose Up
-- The lease's create operations: one per slot (slot 0 the create operation, slot 1 the second attachment's).
ALTER TABLE paperboat.preview_lease_create_operations ADD COLUMN slot smallint NOT NULL DEFAULT 0 CHECK (slot IN (0, 1));
ALTER TABLE paperboat.preview_lease_create_operations DROP CONSTRAINT preview_lease_create_operations_pkey;
ALTER TABLE paperboat.preview_lease_create_operations ADD CONSTRAINT preview_lease_create_operations_pkey PRIMARY KEY (account_id, preview_id, slot);

-- A preview can hold a second carrier attachment on another edge of the workspace's set, so a dead
-- edge costs it neither its URL nor a DNS wait. The second attachment is an ordinary attachment with
-- its own operation (the lease's create operation plus a ".b" suffix) and edge_slot 1.
ALTER TABLE paperboat.preview_lease_carrier_attachments
    ADD COLUMN edge_slot smallint NOT NULL DEFAULT 0 CHECK (edge_slot IN (0, 1));

DROP INDEX paperboat.preview_lease_carrier_attachments_live_preview;
CREATE UNIQUE INDEX preview_lease_carrier_attachments_live_preview
    ON paperboat.preview_lease_carrier_attachments (account_id, preview_id, edge_slot)
    WHERE state <> ALL (ARRAY['failed'::text, 'released'::text]);
CREATE UNIQUE INDEX preview_lease_carrier_attachments_live_edge
    ON paperboat.preview_lease_carrier_attachments (account_id, preview_id, edge_node_id)
    WHERE state <> ALL (ARRAY['failed'::text, 'released'::text]);

-- +goose Down
DELETE FROM paperboat.preview_lease_create_operations WHERE slot = 1;
DROP INDEX paperboat.preview_lease_carrier_attachments_live_edge;
DROP INDEX paperboat.preview_lease_carrier_attachments_live_preview;
UPDATE paperboat.preview_lease_carrier_attachments SET state = 'released', released_at = coalesce(released_at, now()), edge_ready = false, origin_ready = false, ready_at = NULL
    WHERE edge_slot = 1 AND state NOT IN ('failed', 'released');
CREATE UNIQUE INDEX preview_lease_carrier_attachments_live_preview
    ON paperboat.preview_lease_carrier_attachments (account_id, preview_id)
    WHERE state <> ALL (ARRAY['failed'::text, 'released'::text]);
ALTER TABLE paperboat.preview_lease_carrier_attachments DROP COLUMN edge_slot;
ALTER TABLE paperboat.preview_lease_create_operations DROP CONSTRAINT preview_lease_create_operations_pkey;
ALTER TABLE paperboat.preview_lease_create_operations ADD CONSTRAINT preview_lease_create_operations_pkey PRIMARY KEY (account_id, preview_id);
ALTER TABLE paperboat.preview_lease_create_operations DROP COLUMN slot;
