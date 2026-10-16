// Package activity exposes only workspace-scoped administrative event summaries.
// Raw audit metadata and operational support events never cross this boundary.
package activity

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

var ErrInvalid = errors.New("invalid activity filters")

const MaxLimit = 200

type Query struct {
	Cursor, MachineID, Area, Search string
	Limit                           int
}
type Actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
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
type Event struct {
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	Action     string    `json:"action"`
	Actor      Actor     `json:"actor"`
	Target     Target    `json:"target"`
	Machine    *Machine  `json:"machine,omitempty"`
}
type Page struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}
type Service struct{ db *db.DB }

func New(store *db.DB) *Service { return &Service{db: store} }

func (q Query) Validate() error {
	if q.Limit < 0 || q.Limit > MaxLimit || len(q.Cursor) > 512 || len(q.MachineID) > 256 || len(q.Search) > 200 || !utf8.ValidString(q.Search) {
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

func (s *Service) List(ctx context.Context, account string, scope workspace.Scope, q Query) (Page, error) {
	out := Page{Items: []Event{}}
	if err := q.Validate(); err != nil {
		return out, err
	}
	ws := "personal:" + account
	if scope.TeamID != "" {
		ws = "team:" + scope.TeamID
	}
	before, err := parseCursor(ws, q.Cursor)
	if err != nil {
		return out, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = 50
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, account, scope); err != nil {
			return err
		}
		// As with the team administrative audit API, membership alone does not
		// grant visibility into everyone else's administrative actions.
		if scope.TeamID != "" {
			var allowed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams t JOIN team_members m USING(team_id) WHERE t.team_id=$1 AND m.account_id=$2 AND m.active AND (t.owner_account=$2 OR m.role='admin'))`, scope.TeamID, account).Scan(&allowed); err != nil {
				return err
			}
			if !allowed {
				return workspace.ErrForbidden
			}
		}
		rows, err := tx.Query(ctx, `WITH events AS (SELECT *, CASE split_part(event_type,'.',1) WHEN 'auth' THEN 'account' WHEN 'user_machine' THEN 'machine' WHEN 'environment_manifest' THEN 'environment' WHEN 'transfer_destination' THEN 'file' ELSE split_part(event_type,'.',1) END AS area FROM audit_events) SELECT id,created_at,event_type,actor_type,coalesce(actor_user_id,''),resource_type,resource_id,coalesce(activity_target_name,''),coalesce(activity_machine_id,''),coalesce(activity_machine_name,''),cursor_sequence
   FROM events WHERE activity_workspace_id=$1 AND created_at>=now()-interval '90 days'
   AND ($2::bigint=0 OR cursor_sequence<$2)
   AND ($3='' OR activity_machine_id=$3)
   AND ($4='' OR area=ANY(string_to_array($4,',')))
   AND ($5='' OR strpos(lower(event_type||' '||coalesce(activity_target_name,'')),lower($5))>0)
   ORDER BY cursor_sequence DESC LIMIT $6`, ws, before, q.MachineID, q.Area, q.Search, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		var last int64
		for rows.Next() {
			if len(out.Items) == limit {
				out.NextCursor = cursor(ws, last)
				break
			}
			var event Event
			var machine Machine
			if err := rows.Scan(&event.ID, &event.OccurredAt, &event.Action, &event.Actor.Kind, &event.Actor.ID, &event.Target.Kind, &event.Target.ID, &event.Target.Name, &machine.ID, &machine.Name, &last); err != nil {
				return err
			}
			if event.Actor.Kind != "user" {
				event.Actor = Actor{Kind: "system", ID: ""}
			}
			switch event.Target.Kind {
			case "user_machine":
				event.Target.Kind = "machine"
			case "preview_lease":
				event.Target.Kind = "preview"
			}
			if machine.ID != "" {
				event.Machine = &machine
			}
			out.Items = append(out.Items, event)
		}
		return rows.Err()
	})
	return out, err
}
