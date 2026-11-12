package controlplane

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/shared/operationrevocation"
)

// OperationRevocations retains expired admission IDs only while their host watches them.
// Recent revoked credentials also fence new admissions; completed history cannot fill the feed.
func (s *EdgeService) OperationRevocations(ctx context.Context, environment string, live []string) ([]string, error) {
	if environment == "" {
		return nil, operationrevocation.ErrLiveJTIsInvalid
	}
	if err := operationrevocation.ValidateLive(live); err != nil {
		return nil, err
	}
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT jti FROM (
 SELECT c.jti FROM paperboat.control_config_credentials c
 WHERE c.environment_id=$1 AND c.revoked_at IS NOT NULL AND (c.expires_at>$3 OR c.jti=ANY($2::text[]))
 UNION
 SELECT m.helper_terminal_session_id FROM paperboat.user_machine_access_sessions m
 WHERE m.environment_id=$1 AND m.revoked_at IS NOT NULL AND m.helper_terminal_session_id IS NOT NULL
 AND (m.expires_at>$3 OR m.helper_terminal_session_id=ANY($2::text[]))
 UNION
 SELECT m.helper_file_session_id FROM paperboat.user_machine_access_sessions m
 WHERE m.environment_id=$1 AND m.revoked_at IS NOT NULL AND m.helper_file_session_id IS NOT NULL
 AND (m.expires_at>$3 OR m.helper_file_session_id=ANY($2::text[]))
 UNION
 -- A CLI recording replay ticket whose viewer may no longer replay there (recording_view, team membership, CLI
 -- session, account or machine): the host ends the running stream, which the ticket's expiry no longer bounds.
 -- Every host polls this: the machine is resolved once (environment_id is unique) so the admissions are read
 -- through their machine_id index, never a scan of every machine's admissions.
 SELECT a.id FROM paperboat.recording_native_admissions a
 JOIN paperboat.user_machines m ON m.id=a.machine_id
 WHERE a.machine_id=(SELECT id FROM paperboat.user_machines WHERE environment_id=$1) AND (a.expires_at>$3 OR a.id=ANY($2::text[]))
 AND NOT EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c
  JOIN paperboat.users u ON u.id=c.user_id AND u.status='active'
  JOIN paperboat.teams t ON t.team_id=paperboat.recording_team(m.id) AND t.deleted_at IS NULL AND NOT t.suspended
  WHERE c.id=a.native_cli_session_id AND c.user_id=a.account_id AND c.state='active' AND c.revoked_at IS NULL
  AND m.revoked_at IS NULL AND m.deleted_at IS NULL
  AND NOT isempty(paperboat.workspace_policy_rule_ports(a.account_id, t.team_id, a.machine_id, 'recording_view')))) revoked ORDER BY jti LIMIT $4`, environment, live, s.clock().UTC(), operationrevocation.MaxLiveJTIs+1)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		result = append(result, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(result) > operationrevocation.MaxLiveJTIs {
		return nil, errors.New("operation revocation feed capacity exceeded")
	}
	return result, nil
}
