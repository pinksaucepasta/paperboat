// Package activity is the workspace audit log (PLAN F19-07..09): what happened in a personal or
// team workspace, who did it, the result, and only documented details. Raw audit metadata and
// operational support events never cross this boundary.
package activity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

var ErrInvalid = errors.New("invalid activity filters")

const MaxLimit = 200

// Query filters the audit log. Every filter is optional; Since/Until bound the time range.
type Query struct {
	Cursor, MachineID, Area, Search string
	Actor, Action, Target, Result   string
	Since, Until                    time.Time
	Limit                           int
}
type Actor struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Email   string `json:"email,omitempty"`
	TokenID string `json:"token_id,omitempty"`
}
type Target struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}
type Machine struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Event is the documented audit event shape (server/docs/teams-business.md), the same in the
// API, the export and the stream.
type Event struct {
	ID         string         `json:"id"`
	OccurredAt time.Time      `json:"occurred_at"`
	Workspace  string         `json:"workspace"`
	Action     string         `json:"action"`
	Actor      Actor          `json:"actor"`
	Target     Target         `json:"target"`
	Machine    *Machine       `json:"machine,omitempty"`
	Result     string         `json:"result"`
	Reason     string         `json:"reason,omitempty"`
	IP         string         `json:"ip,omitempty"`
	UserAgent  string         `json:"user_agent,omitempty"`
	RequestID  string         `json:"request_id,omitempty"`
	Details    map[string]any `json:"details"`
	sequence   int64
}
type Page struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}
type Service struct {
	db  *db.DB
	cfg config.Business
}

func New(store *db.DB, cfg config.Business) *Service { return &Service{db: store, cfg: cfg} }

func (q Query) Validate() error {
	if q.Limit < 0 || q.Limit > MaxLimit || len(q.Cursor) > 512 || len(q.MachineID) > 256 || len(q.Search) > 200 || !utf8.ValidString(q.Search) {
		return ErrInvalid
	}
	if len(q.Actor) > 320 || len(q.Action) > 128 || len(q.Target) > 256 || !utf8.ValidString(q.Actor+q.Action+q.Target) {
		return ErrInvalid
	}
	if q.Result != "" && q.Result != "success" && q.Result != "denied" {
		return ErrInvalid
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && q.Until.Before(q.Since) {
		return ErrInvalid
	}
	for _, area := range strings.Split(q.Area, ",") {
		switch area {
		case "", "machine", "terminal", "preview", "tunnel", "file", "environment", "config", "team", "share", "account":
		default:
			return ErrInvalid
		}
	}
	return nil
}
func cursor(scope string, sequence int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(scope + ":" + strconv.FormatInt(sequence, 10)))
}
func parseCursor(scope, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	prefix := scope + ":"
	if err != nil || !strings.HasPrefix(string(raw), prefix) {
		return 0, ErrInvalid
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(string(raw), prefix), 10, 64)
	if err != nil || n <= 0 || cursor(scope, n) != value {
		return 0, ErrInvalid
	}
	return n, nil
}

func workspaceID(account string, scope workspace.Scope) string {
	if scope.TeamID != "" {
		return "team:" + scope.TeamID
	}
	return "personal:" + account
}

// authorizeTx checks that account may read this workspace's audit log: the owner of a personal
// workspace, or a team member holding audit.read. It returns the workspace's retention.
func (s *Service) authorizeTx(ctx context.Context, tx *db.Tx, account string, scope workspace.Scope) (time.Duration, error) {
	if err := workspace.ValidateTx(ctx, tx, account, scope); err != nil {
		return 0, err
	}
	if scope.TeamID != "" {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams t JOIN team_members m USING(team_id) WHERE t.team_id=$1 AND m.account_id=$2 AND m.active AND team_role_allows(CASE WHEN t.owner_account=$2 THEN 'owner' ELSE m.role END,'audit.read'))`, scope.TeamID, account).Scan(&allowed); err != nil {
			return 0, err
		}
		if !allowed {
			return 0, workspace.ErrForbidden
		}
	}
	var class string
	if err := tx.QueryRow(ctx, `SELECT audit_retention_class($1)`, workspaceID(account, scope)).Scan(&class); err != nil {
		return 0, err
	}
	return s.retention(class), nil
}

func (s *Service) retention(class string) time.Duration {
	switch class {
	case "team":
		return s.cfg.AuditRetentionTeam
	case "pro":
		return s.cfg.AuditRetentionPro
	}
	return s.cfg.AuditRetentionFree
}

// eventsSQL selects one workspace's events newest first. $1 workspace, $2 retention seconds,
// $3 cursor, $4 machine, $5 areas, $6 search, $7 actor, $8 action prefix, $9 target,
// $10 result, $11 since, $12 until, $13 row limit.
const eventsSQL = `WITH events AS (SELECT *, CASE split_part(event_type,'.',1) WHEN 'auth' THEN 'account' WHEN 'user_machine' THEN 'machine' WHEN 'environment_manifest' THEN 'environment' WHEN 'transfer_destination' THEN 'file' ELSE split_part(event_type,'.',1) END AS area FROM audit_events WHERE activity_workspace_id=$1)
 SELECT id,created_at,event_type,actor_type,coalesce(actor_user_id,''),coalesce(actor_email,''),coalesce(actor_token_id,''),
  resource_type,resource_id,coalesce(activity_target_name,''),coalesce(activity_machine_id,''),coalesce(activity_machine_name,''),
  result,coalesce(reason,''),coalesce(client_ip,''),coalesce(user_agent,''),metadata,cursor_sequence
 FROM events WHERE created_at>=now()-make_interval(secs=>$2)
  AND ($3::bigint=0 OR cursor_sequence<$3)
  AND ($4='' OR activity_machine_id=$4)
  AND ($5='' OR area=ANY(string_to_array($5,',')))
  AND ($6='' OR strpos(lower(event_type||' '||coalesce(activity_target_name,'')||' '||coalesce(actor_email,'')),lower($6))>0)
  AND ($7='' OR actor_user_id=$7 OR lower(actor_email)=lower($7) OR actor_token_id=$7)
  AND ($8='' OR event_type=$8 OR starts_with(event_type,$8||'.'))
  AND ($9='' OR resource_id=$9 OR activity_machine_id=$9)
  AND ($10='' OR result=$10)
  AND ($11::timestamptz IS NULL OR created_at>=$11)
  AND ($12::timestamptz IS NULL OR created_at<$12)
 ORDER BY cursor_sequence DESC LIMIT $13`

func optionalTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// scan reads events newest first, calling emit for each until it returns false.
func scan(ctx context.Context, tx *db.Tx, ws string, retention time.Duration, before int64, q Query, rowLimit int, emit func(Event) bool) (int64, error) {
	rows, err := tx.Query(ctx, eventsSQL, ws, retention.Seconds(), before, q.MachineID, q.Area, q.Search, q.Actor, q.Action, q.Target, q.Result, optionalTime(q.Since), optionalTime(q.Until), rowLimit)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		event, err := decode(rows, ws)
		if err != nil {
			return 0, err
		}
		last = event.sequence
		if !emit(event) {
			break
		}
	}
	return last, rows.Err()
}

// decode reads one event row in the column order of eventsSQL.
func decode(rows interface{ Scan(...any) error }, ws string) (Event, error) {
	event := Event{Workspace: ws}
	var machine Machine
	var actorType string
	var metadata []byte
	if err := rows.Scan(&event.ID, &event.OccurredAt, &event.Action, &actorType, &event.Actor.ID, &event.Actor.Email, &event.Actor.TokenID,
		&event.Target.Kind, &event.Target.ID, &event.Target.Name, &machine.ID, &machine.Name,
		&event.Result, &event.Reason, &event.IP, &event.UserAgent, &metadata, &event.sequence); err != nil {
		return event, err
	}
	event.Actor.Kind = actorKind(actorType, event.Actor.TokenID)
	if event.Actor.Kind == "system" {
		event.Actor = Actor{Kind: "system"}
	}
	switch event.Target.Kind {
	case "user_machine", "machine_access":
		event.Target.Kind = "machine"
	case "preview_lease":
		event.Target.Kind = "preview"
	}
	if machine.ID != "" {
		event.Machine = &machine
	}
	var raw map[string]any
	if json.Unmarshal(metadata, &raw) == nil {
		event.RequestID, _ = raw["request_id"].(string)
	}
	event.Details = teams.ProjectActivityMetadata(raw)
	delete(event.Details, "request_id")
	return event, nil
}

// actorKind maps the stored actor type to the documented one.
func actorKind(actorType, tokenID string) string {
	switch {
	case actorType == "user" && tokenID != "":
		return "api_token"
	case actorType == "user", actorType == "directory":
		return actorType
	case actorType == "admin":
		return "operator"
	}
	return "system"
}

func (s *Service) List(ctx context.Context, account string, scope workspace.Scope, q Query) (Page, error) {
	out := Page{Items: []Event{}}
	if err := q.Validate(); err != nil {
		return out, err
	}
	ws := workspaceID(account, scope)
	before, err := parseCursor(ws, q.Cursor)
	if err != nil {
		return out, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = 50
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		retention, err := s.authorizeTx(ctx, tx, account, scope)
		if err != nil {
			return err
		}
		_, err = scan(ctx, tx, ws, retention, before, q, limit+1, func(e Event) bool {
			if len(out.Items) == limit {
				out.NextCursor = cursor(ws, out.Items[limit-1].sequence)
				return false
			}
			out.Items = append(out.Items, e)
			return true
		})
		return err
	})
	return out, err
}
