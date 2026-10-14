-- name: DeviceReplaySession :one
SELECT s.id FROM cli_client_sessions s
JOIN cli_access_tokens a ON a.cli_client_session_id = s.id
JOIN cli_refresh_tokens r ON r.cli_client_session_id = s.id
WHERE s.id = sqlc.arg(session_id) AND s.user_id = sqlc.arg(user_id) AND s.state = 'active'
AND a.token_hash = ANY(string_to_array(sqlc.arg(access_hashes)::text, ' ')) AND a.revoked_at IS NULL AND a.expires_at > sqlc.arg(now_at)
AND r.token_hash = ANY(string_to_array(sqlc.arg(refresh_hashes)::text, ' ')) AND r.state = 'active' AND r.revoked_at IS NULL AND r.expires_at > sqlc.arg(now_at)
FOR SHARE OF s, a, r;

