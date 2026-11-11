-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A preview dispatch that times out or loses its connection marks the create operation uncertain in phase 'dispatch'
-- (MarkPreviewLeaseCreateOperationUncertainV1); the baseline check did not list it, so that write failed on top of the
-- 503 it reports. The new check is added NOT VALID and validated in 292, so this statement holds its lock only briefly.
ALTER TABLE paperboat.operations
    DROP CONSTRAINT operations_phase_check,
    ADD CONSTRAINT operations_phase_check CHECK (phase = ANY (ARRAY['validating', 'persisting', 'waiting_for_dns', 'issuing_certificate', 'installing_service', 'connecting', 'checking_origin', 'dispatch', 'draining', 'rolling_back', 'ready', 'failed'])) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.operations
    DROP CONSTRAINT operations_phase_check,
    ADD CONSTRAINT operations_phase_check CHECK (phase = ANY (ARRAY['validating', 'persisting', 'waiting_for_dns', 'issuing_certificate', 'installing_service', 'connecting', 'checking_origin', 'draining', 'rolling_back', 'ready', 'failed'])) NOT VALID;
