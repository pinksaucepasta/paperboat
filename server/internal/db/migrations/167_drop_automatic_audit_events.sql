-- +goose Up
-- Config sync credentials and repository access, sync status reports and a preview owner's lease renewals were
-- audit events on every automatic sync and renewal: 39.8k rows, 80% of audit_events on 2026-10-23, none
-- of it something a person did. Servers no longer write them (a person's own preview renewal still is one); the
-- rows written so far go here, once. Stragglers from servers still rolling out age out with the audit retention.
SELECT set_config('paperboat.audit_prune', 'on', true);
DELETE FROM paperboat.audit_events
WHERE event_type IN ('config.credential_issued', 'config.repository_access_issued', 'config.status_recorded')
   OR (event_type = 'preview.lease_renewed' AND actor_type <> 'user');

-- +goose Down
-- The deleted rows are not restored.
SELECT 1;
