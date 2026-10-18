-- name: RetireExpiredMachineAccessSessions :execrows
-- Admitted streams keep their authority beyond the credential TTL until explicit revocation, so an
-- expired session is only retired after a retention period; the helper side expired it long before.
UPDATE user_machine_access_sessions
SET state = 'revoked', revoked_at = now(), revocation_reason = 'expired',
    helper_revoked_at = coalesce(helper_revoked_at, now()), updated_at = now()
WHERE state = 'active' AND expires_at < sqlc.arg(expired_before);
