-- name: FindLiveOperationAccessSession :one
SELECT id, helper_terminal_session_id, expires_at
FROM user_machine_access_sessions
WHERE operation_key = sqlc.arg(operation_key) AND state = 'active' AND expires_at > sqlc.arg(issued_at)
ORDER BY expires_at DESC LIMIT 1;

-- name: ReleaseOperationAccessSessions :exec
UPDATE user_machine_access_sessions
SET state = 'revoked', revoked_at = now(), revocation_reason = 'operation_ended',
    helper_revoked_at = now(), updated_at = now()
WHERE id = ANY(ARRAY(
  SELECT session.id FROM user_machine_access_sessions AS session
  WHERE session.user_id = sqlc.arg(user_id) AND session.cli_client_session_id = sqlc.arg(cli_client_session_id)
    AND session.user_machine_id = sqlc.arg(machine_id) AND session.operation_id = sqlc.arg(operation_id)
    AND session.capabilities = ARRAY[sqlc.arg(capability)::text] AND session.operation_key IS NOT NULL
    AND session.state = 'active'
  ORDER BY session.id FOR UPDATE
));
