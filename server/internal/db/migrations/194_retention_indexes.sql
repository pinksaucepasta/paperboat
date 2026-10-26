-- +goose NO TRANSACTION
-- +goose Up
-- The retention worker (internal/retention) now bounds every per-operation table (F21-23). Each new policy finds
-- its candidates by one of these indexes, so a pass that has nothing to delete reads a few index pages, not the
-- table; partial where only finished rows are candidates.
CREATE INDEX CONCURRENTLY IF NOT EXISTS browser_machine_credentials_expiry ON paperboat.browser_machine_credentials (expires_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS inspector_credentials_expiry ON paperboat.inspector_credentials (expires_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS control_operations_completed ON paperboat.control_operations (completed_at) WHERE completed_at IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS preview_carrier_attachment_outbox_finished ON paperboat.preview_carrier_attachment_outbox (updated_at)
    WHERE state IN ('delivered', 'failed');
CREATE INDEX CONCURRENTLY IF NOT EXISTS tunnel_connector_enrollments_expiry ON paperboat.tunnel_connector_enrollments (expires_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS transactional_mail_outbox_finished ON paperboat.transactional_mail_outbox (created_at)
    WHERE state <> 'queued';
CREATE INDEX CONCURRENTLY IF NOT EXISTS managed_ssh_client_keys_revoked ON paperboat.managed_ssh_client_keys (revoked_at) WHERE state = 'revoked';
CREATE INDEX CONCURRENTLY IF NOT EXISTS tunnel_certificate_records_finished ON paperboat.tunnel_certificate_records (updated_at)
    WHERE state IN ('superseded', 'revoked', 'expired', 'failed');
CREATE INDEX CONCURRENTLY IF NOT EXISTS peer_endpoint_certificates_expired ON paperboat.peer_endpoint_certificates (expires_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS peer_endpoint_certificate_revocations_certificate ON paperboat.peer_endpoint_certificate_revocations (certificate_fingerprint);
CREATE INDEX CONCURRENTLY IF NOT EXISTS peer_endpoint_certificate_operations_certificate ON paperboat.peer_endpoint_certificate_operations (certificate_fingerprint);
CREATE INDEX CONCURRENTLY IF NOT EXISTS peer_mesh_log_recorded ON paperboat.peer_mesh_log (recorded_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.peer_mesh_log_recorded;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.peer_endpoint_certificate_operations_certificate;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.peer_endpoint_certificate_revocations_certificate;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.peer_endpoint_certificates_expired;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_certificate_records_finished;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.managed_ssh_client_keys_revoked;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.transactional_mail_outbox_finished;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_connector_enrollments_expiry;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_carrier_attachment_outbox_finished;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_operations_completed;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.inspector_credentials_expiry;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.browser_machine_credentials_expiry;
