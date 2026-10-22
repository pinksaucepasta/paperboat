-- +goose Up
-- Before machine revocation ended the CLI session `pb pair` created for the machine (91aeec3d), a
-- disconnected or deleted machine left that session active. Each such session still counted as an account
-- trust key sent to every machine, and the set outgrew what clients stored. End those sessions now with the
-- same fence the revocation path applies; the session triggers revoke their keys and peer identities.
UPDATE paperboat.cli_access_tokens AS access_token
SET revoked_at = coalesce(access_token.revoked_at, now())
FROM paperboat.cli_client_sessions AS client_session
JOIN paperboat.user_machines AS machine ON machine.id = client_session.user_machine_id AND machine.user_id = client_session.user_id
WHERE access_token.cli_client_session_id = client_session.id AND access_token.revoked_at IS NULL
  AND client_session.state = 'active'
  AND (machine.state IN ('revoked', 'disconnected', 'deleted') OR machine.revoked_at IS NOT NULL OR machine.deleted_at IS NOT NULL);
UPDATE paperboat.cli_refresh_tokens AS refresh_token
SET state = 'revoked', revoked_at = coalesce(refresh_token.revoked_at, now())
FROM paperboat.cli_client_sessions AS client_session
JOIN paperboat.user_machines AS machine ON machine.id = client_session.user_machine_id AND machine.user_id = client_session.user_id
WHERE refresh_token.cli_client_session_id = client_session.id AND refresh_token.state <> 'revoked'
  AND client_session.state = 'active'
  AND (machine.state IN ('revoked', 'disconnected', 'deleted') OR machine.revoked_at IS NOT NULL OR machine.deleted_at IS NOT NULL);
UPDATE paperboat.cli_client_sessions AS client_session
SET state = 'revoked', revoked_at = coalesce(client_session.revoked_at, now()),
    revocation_reason = coalesce(client_session.revocation_reason, 'machine_removed'), version = client_session.version + 1
FROM paperboat.user_machines AS machine
WHERE machine.id = client_session.user_machine_id AND machine.user_id = client_session.user_id
  AND client_session.state = 'active'
  AND (machine.state IN ('revoked', 'disconnected', 'deleted') OR machine.revoked_at IS NOT NULL OR machine.deleted_at IS NOT NULL);

-- +goose Down
-- Ended sessions stay ended.
SELECT 1;
