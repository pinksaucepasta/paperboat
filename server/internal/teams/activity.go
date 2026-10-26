package teams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// DefaultActivityLimit bounds administrative metadata pages. The fixed 90-day retention boundary
// in queries below matches migration 185's audit-integrity trigger; it is not
// a runtime setting or a resource lifetime.
const DefaultActivityLimit = 50
const MaximumActivityLimit = 200
const MaximumActivityMetadataBytes = 4096

type ActivityItem struct {
	ID           string         `json:"id"`
	ActorAccount string         `json:"actor_account"`
	Action       string         `json:"action"`
	CreatedAt    time.Time      `json:"created_at"`
	Metadata     map[string]any `json:"metadata"`
}
type ActivityPage struct {
	Items      []ActivityItem `json:"items"`
	NextCursor string         `json:"next_cursor"`
}

func activityCursor(team string, sequence int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(team + ":" + strconv.FormatInt(sequence, 10)))
}
func parseActivityCursor(team, cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > 512 {
		return 0, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrInvalid
	}
	prefix := team + ":"
	if !strings.HasPrefix(string(raw), prefix) {
		return 0, ErrInvalid
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(string(raw), prefix), 10, 64)
	if err != nil || n <= 0 || activityCursor(team, n) != cursor {
		return 0, ErrInvalid
	}
	return n, nil
}

// activityMetadata projects known metadata only. Historical unknown keys are never returned.
func activityMetadata(input map[string]any, strict bool) (map[string]any, error) {
	out := make(map[string]any)
	for key, value := range input {
		switch key {
		case "generation", "epoch", "revision", "target_account", "recipient_account", "sender_account", "role", "resource_kind", "resource_id", "permission", "active", "machine_id", "audience", "capabilities", "terminal_session_id", "action", "access_session_id", "attachment_id", "request_id", "source_machine_id", "destination_machine_id", "status", "decision", "repository_id", "version", "published_revision", "assignment_id", "default_version", "previous_owner", "new_owner", "adopted_version", "format", "rows", "since", "until", "tax_id", "intent", "required", "session_seconds", "jit", "exempt", "group_id", "mapped_only", "reason", "gained", "lost", "api_token", "kind", "capability", "policy_version", "origin", "session_id", "outcome", "bytes_in", "bytes_out", "peer_endpoint_id", "peer_account_id", "scope_generation", "operation_id":
		default:
			if strict {
				return nil, ErrInvalid
			}
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil || len(raw) > 1024 {
			return nil, ErrInvalid
		}
		switch v := value.(type) {
		case string:
			if len(v) > 512 || strings.ContainsAny(v, "\r\n\x00") {
				return nil, ErrInvalid
			}
		case bool, int, int32, int64, uint, uint32, uint64, float64, json.Number:
		case []string:
			if len(v) > 32 {
				return nil, ErrInvalid
			}
			for _, s := range v {
				if len(s) > 64 || strings.ContainsAny(s, "\r\n\x00") {
					return nil, ErrInvalid
				}
			}
		case []any:
			if key != "capabilities" || len(v) > 32 {
				return nil, ErrInvalid
			}
			for _, item := range v {
				s, ok := item.(string)
				if !ok || len(s) > 64 || strings.ContainsAny(s, "\r\n\x00") {
					return nil, ErrInvalid
				}
			}
		default:
			return nil, ErrInvalid
		}
		out[key] = value
	}
	raw, err := json.Marshal(out)
	if err != nil || len(raw) > MaximumActivityMetadataBytes {
		return nil, ErrInvalid
	}
	return out, nil
}

// ProjectActivityMetadata returns only the documented metadata keys of an audit event, for
// the audit log's details. Unknown or malformed values are dropped, never returned.
func ProjectActivityMetadata(input map[string]any) map[string]any {
	out, err := activityMetadata(input, false)
	if err != nil {
		return map[string]any{}
	}
	return out
}

func (s *Service) Activity(ctx context.Context, account, team, cursor string, limit int) (ActivityPage, error) {
	out := ActivityPage{Items: []ActivityItem{}}
	if !validID(team) || limit < 0 || limit > MaximumActivityLimit {
		return out, ErrInvalid
	}
	if limit == 0 {
		limit = DefaultActivityLimit
	}
	before, err := parseActivityCursor(team, cursor)
	if err != nil {
		return out, err
	}
	// One read-only snapshot authorizes and reads the page: the caller's own membership is all it needs, not the
	// whole team or the server-wide team lock.
	err = s.db.InReadOnlySnapshot(ctx, func(ctx context.Context, tx *db.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `SELECT CASE WHEN t.owner_account=m.account_id THEN 'owner' ELSE m.role END
 FROM teams t JOIN team_members m ON m.team_id=t.team_id AND m.account_id=$2 AND m.active
 WHERE t.team_id=$1 AND t.deleted_at IS NULL AND NOT t.suspended`, team, account).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !Allowed(role, PermissionAuditRead) {
			return ErrForbidden
		}
		rows, err := tx.Query(ctx, `SELECT id,coalesce(actor_user_id,''),event_type,created_at,metadata,cursor_sequence FROM audit_events WHERE resource_type='team' AND resource_id=$1 AND created_at>=now()-interval '90 days' AND ($2::bigint=0 OR cursor_sequence<$2) ORDER BY cursor_sequence DESC LIMIT $3`, team, before, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		var last int64
		for rows.Next() {
			if len(out.Items) == limit {
				out.NextCursor = activityCursor(team, last)
				break
			}
			var item ActivityItem
			var raw []byte
			if err := rows.Scan(&item.ID, &item.ActorAccount, &item.Action, &item.CreatedAt, &raw, &last); err != nil {
				return err
			}
			var metadata map[string]any
			if err := json.Unmarshal(raw, &metadata); err != nil {
				return err
			}
			item.Metadata, err = activityMetadata(metadata, false)
			if err != nil {
				return err
			}
			item.Action = strings.TrimPrefix(item.Action, "team.")
			out.Items = append(out.Items, item)
		}
		return rows.Err()
	})
	return out, err
}
