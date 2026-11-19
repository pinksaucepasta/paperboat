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
