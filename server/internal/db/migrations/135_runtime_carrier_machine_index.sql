-- +goose Up
-- The endpoint engine reads an agent's runtime carriers by machine (reconcile on every attachment
-- and preview create, endpoint admissions, readiness). Without an index those reads scan the table
-- and, under serializable isolation, conflict with every edge observation write.
CREATE INDEX runtime_carrier_attachments_machine ON paperboat.runtime_carrier_attachments(machine_id);

-- +goose Down
DROP INDEX paperboat.runtime_carrier_attachments_machine;
