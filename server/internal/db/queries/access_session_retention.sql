-- name: RetireExpiredMachineAccessSessions :execrows
-- Admitted streams keep their authority beyond the credential TTL until explicit revocation, so an expired session
-- stays while a client still names it (admitted_seen_at, peersessions markAdmittedSessions). A signed one that nobody
-- named since idle_before (a whole peer document lifetime) has no open stream left and is retired; any session is
-- retired after the retention period (expired_before); the helper side expired it long before.
-- One bounded batch per statement: one statement over every expired row of a busy machine held its locks for seconds.
UPDATE user_machine_access_sessions
SET state = 'revoked', revoked_at = now(), revocation_reason = 'expired',
    helper_revoked_at = coalesce(helper_revoked_at, now()), updated_at = now()
WHERE id IN (
  SELECT due.id FROM user_machine_access_sessions AS due
  WHERE due.state = 'active' AND due.expires_at < sqlc.arg(idle_before)
    AND (due.expires_at < sqlc.arg(expired_before) OR (due.credential_kind = 'control_plane_signed'
      AND (due.admitted_seen_at IS NULL OR due.admitted_seen_at < sqlc.arg(idle_before))))
  ORDER BY due.expires_at LIMIT sqlc.arg(batch_size)::int FOR UPDATE SKIP LOCKED);

-- name: RetireAccessSessionsOfEndedMachines :execrows
-- The expired signed sessions a machine revoke leaves active (migration 443; they need no helper call), one bounded batch per statement.
UPDATE user_machine_access_sessions
SET state = 'revoked', revoked_at = now(), revocation_reason = 'machine_ended',
    helper_revoked_at = coalesce(helper_revoked_at, now()), updated_at = now()
WHERE id IN (
  -- Driven by the ended machines (few), then one range scan of user_machine_access_sessions_machine_active_expiry per
  -- machine: a plan that cannot depend on how many sessions the statistics believe there are.
  SELECT due.id FROM user_machines AS machine
  CROSS JOIN LATERAL (
    SELECT session.id FROM user_machine_access_sessions AS session
    -- No order (every expired session of an ended machine goes): user_machine_access_sessions_machine_active_expiry
    -- matches both conditions, so no plan can prefer the global active-expiry index (which, to give an expiry order,
    -- fresh statistics picked and walked every account's expired sessions for each machine).
    WHERE session.user_machine_id = machine.id AND session.state = 'active' AND session.expires_at <= now()
      AND session.credential_kind = 'control_plane_signed'
    LIMIT sqlc.arg(batch_size)::int
    FOR UPDATE OF session SKIP LOCKED) AS due
  WHERE machine.revoked_at IS NOT NULL OR machine.deleted_at IS NOT NULL OR machine.state IN ('revoked', 'disconnected', 'deleted')
  LIMIT sqlc.arg(batch_size)::int);
