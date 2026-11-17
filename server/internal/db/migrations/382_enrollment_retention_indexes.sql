-- +goose NO TRANSACTION
-- +goose Up
-- The retention worker deletes finished helper enrollments, machine pairings, pairing reset receipts and account deletion
-- jobs, 500 per statement; these indexes keep each batch off a full scan.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_helper_enrollments_expired;
CREATE INDEX CONCURRENTLY control_helper_enrollments_expired ON paperboat.control_helper_enrollments (expires_at);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_pairings_finished;
CREATE INDEX CONCURRENTLY user_machine_pairings_finished ON paperboat.user_machine_pairings (expires_at)
  WHERE installation_state IN ('cancelled','expired','denied','revoked','deleted');
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_pairing_resets_completed;
CREATE INDEX CONCURRENTLY user_machine_pairing_resets_completed ON paperboat.user_machine_pairing_resets (completed_at);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.account_deletion_jobs_finished;
CREATE INDEX CONCURRENTLY account_deletion_jobs_finished ON paperboat.account_deletion_jobs ((coalesce(completed_at,created_at)))
  WHERE state <> 'queued';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_helper_enrollments_expired;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_pairings_finished;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_pairing_resets_completed;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.account_deletion_jobs_finished;
