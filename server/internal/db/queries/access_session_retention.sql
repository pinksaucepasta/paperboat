-- name: RetireExpiredMachineAccessSessions :execrows
-- Admitted streams keep their authority beyond the credential TTL until explicit revocation, so an
-- expired session is only retired after a retention period; the helper side expired it long before.
-- One bounded batch per statement: one statement over every expired row of a busy machine held its locks for seconds.
UPDATE user_machine_access_sessions
SET state = 'revoked', revoked_at = now(), revocation_reason = 'expired',
    helper_revoked_at = coalesce(helper_revoked_at, now()), updated_at = now()
WHERE id IN (
  SELECT due.id FROM user_machine_access_sessions AS due
  WHERE due.state = 'active' AND due.expires_at < sqlc.arg(expired_before)
  ORDER BY due.expires_at LIMIT sqlc.arg(batch_size)::int FOR UPDATE SKIP LOCKED);

-- name: RetireAccessSessionsOfEndedMachines :execrows
-- The expired signed sessions a machine revoke leaves active (migration 443; they need no helper call), one bounded batch per statement.
UPDATE user_machine_access_sessions
SET state = 'revoked', revoked_at = now(), revocation_reason = 'machine_ended',
    helper_revoked_at = coalesce(helper_revoked_at, now()), updated_at = now()
WHERE id IN (
  SELECT due.id FROM user_machine_access_sessions AS due
  JOIN user_machines AS machine ON machine.id = due.user_machine_id
  WHERE due.state = 'active' AND due.expires_at <= now() AND due.credential_kind = 'control_plane_signed'
    AND (machine.revoked_at IS NOT NULL OR machine.deleted_at IS NOT NULL OR machine.state IN ('revoked', 'disconnected', 'deleted'))
  LIMIT sqlc.arg(batch_size)::int FOR UPDATE OF due SKIP LOCKED);
