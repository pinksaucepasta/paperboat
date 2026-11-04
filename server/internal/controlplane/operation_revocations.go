package controlplane

import (
	"context"
	"errors"
	"strings"
)

// MaxLiveOperationJTIs bounds a host's watches and its revocation response.
const MaxLiveOperationJTIs = 1024

var ErrLiveOperationJTIsInvalid = errors.New("active operation credential IDs are invalid")

// OperationRevocations retains expired admission IDs only while their host watches them.
// Recent revoked credentials also fence new admissions; completed history cannot fill the feed.
func (s *EdgeService) OperationRevocations(ctx context.Context, environment string, live []string) ([]string, error) {
	if environment == "" || live == nil || len(live) > MaxLiveOperationJTIs {
		return nil, ErrLiveOperationJTIsInvalid
	}
	seen := make(map[string]bool, len(live))
	for _, id := range live {
		if id == "" || len(id) > 128 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n") || seen[id] {
			return nil, ErrLiveOperationJTIsInvalid
		}
		seen[id] = true
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
 AND (m.expires_at>$3 OR m.helper_file_session_id=ANY($2::text[]))) revoked ORDER BY jti LIMIT $4`, environment, live, s.clock().UTC(), MaxLiveOperationJTIs+1)
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
	if len(result) > MaxLiveOperationJTIs {
		return nil, errors.New("operation revocation feed capacity exceeded")
	}
	return result, nil
}
