package usermachines

import "context"

// ReleaseFileTransferSessions ends only the caller's dedicated file-transfer credentials.
// Terminal descriptors can also carry files and have a separate lifetime.
func (s *Service) ReleaseFileTransferSessions(ctx context.Context, userID, cliSessionID string, ids []string) error {
	_, err := s.db.SQL().ExecContext(ctx, `
UPDATE paperboat.user_machine_access_sessions
SET state='revoked', revoked_at=now(), revocation_reason='transfer_finished',
    helper_revoked_at=now(), updated_at=now()
WHERE id IN (
    SELECT id FROM paperboat.user_machine_access_sessions
    WHERE id=ANY($1::text[]) AND user_id=$2 AND cli_client_session_id=$3
      AND state='active' AND credential_kind='control_plane_signed'
      AND capabilities=ARRAY['files'] AND helper_file_session_id IS NOT NULL
      AND helper_terminal_session_id IS NULL
    ORDER BY id FOR UPDATE
)`, ids, userID, cliSessionID)
	return err
}
