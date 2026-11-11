package activity

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// exportPageRows is how many events one export transaction reads.
const exportPageRows = 500

// ErrExportNeedsTeam means export is a Team plan feature (PLAN F19, user decision 2026-10-23).
var ErrExportNeedsTeam = errors.New("audit log export is available in team workspaces")

// CSVHeader is the export's column order; the documented event shape flattened.
var CSVHeader = []string{"id", "occurred_at", "workspace", "action", "result", "reason", "actor_kind", "actor_id", "actor_email", "actor_token_id", "target_kind", "target_id", "target_name", "machine_id", "machine_name", "ip", "user_agent", "request_id", "details"}

// Export writes the workspace's events matching q (newest first, at most AuditExportMaxRows) to
// w as "csv" or "json" (one JSON array), then records the export in the audit log. Authorization
// is re-checked for every page, in that page's short transaction: it takes the team lock, which must
// never be held while a download streams to a slow client.
func (s *Service) Export(ctx context.Context, account string, scope workspace.Scope, q Query, format string, w io.Writer) error {
	if format != "csv" && format != "json" {
		return ErrInvalid
	}
	q.Cursor, q.Limit = "", 0
	if err := q.Validate(); err != nil {
		return err
	}
	if scope.TeamID == "" {
		return ErrExportNeedsTeam
	}
	ws := workspaceID(account, scope)
	// Each page re-checks membership, the audit.read permission and the retention class in its own transaction (the
	// team lock is taken there, never while a download streams), so access ended mid-export ends the export.
	// Pages of exportPageRows are read by cursor and written after the transaction ends: a slow client never pins a
	// pool slot or an old snapshot (F21-41).
	var err error
	rows := 0
	var out *encoder // created once the first page is authorized: the CSV header is written with it (AuditExportMaxRows >= 1)
	var before int64
	for rows < s.cfg.AuditExportMaxRows {
		want := min(exportPageRows, s.cfg.AuditExportMaxRows-rows)
		page := make([]Event, 0, want)
		var last int64
		if err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			page = page[:0]
			retention, err := s.authorizeTx(ctx, tx, account, scope)
			if err != nil {
				return err
			}
			last, err = scan(ctx, tx, ws, retention, before, q, want, func(e Event) bool {
				page = append(page, e)
				return true
			})
			return err
		}); err != nil {
			return err
		}
		if out == nil {
			out = newEncoder(format, w)
		}
		for _, e := range page {
			if err = out.event(e); err != nil {
				return err
			}
		}
		rows += len(page)
		before = last
		if len(page) < want {
			break
		}
	}
	err = out.close()
	if err != nil {
		return err
	}
	return audit.NewWriter(s.db).Write(ctx, audit.Event{ActorUserID: account, ActorType: audit.ActorUser, EventType: "team.activity_exported", ResourceType: "team", ResourceID: scope.TeamID,
		Metadata: map[string]any{"format": format, "rows": rows, "action": q.Action, "since": timeText(q.Since), "until": timeText(q.Until)}})
}

func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

type encoder struct {
	csv   *csv.Writer
	json  io.Writer
	first bool
}

func newEncoder(format string, w io.Writer) *encoder {
	if format == "csv" {
		c := csv.NewWriter(w)
		_ = c.Write(CSVHeader)
		return &encoder{csv: c}
	}
	return &encoder{json: w, first: true}
}

func (e *encoder) event(ev Event) error {
	if e.csv != nil {
		details, _ := json.Marshal(ev.Details)
		machineID, machineName := "", ""
		if ev.Machine != nil {
			machineID, machineName = ev.Machine.ID, ev.Machine.Name
		}
		return e.csv.Write(csvSafe([]string{ev.ID, ev.OccurredAt.UTC().Format(time.RFC3339Nano), ev.Workspace, ev.Action, ev.Result, ev.Reason,
			ev.Actor.Kind, ev.Actor.ID, ev.Actor.Email, ev.Actor.TokenID, ev.Target.Kind, ev.Target.ID, ev.Target.Name,
			machineID, machineName, ev.IP, ev.UserAgent, ev.RequestID, string(details)}))
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	prefix := ","
	if e.first {
		prefix, e.first = "[", false
	}
	_, err = e.json.Write(append([]byte(prefix), raw...))
	return err
}

func (e *encoder) close() error {
	if e.csv != nil {
		e.csv.Flush()
		return e.csv.Error()
	}
	end := "]"
	if e.first {
		end = "[]"
	}
	_, err := io.WriteString(e.json, end+"\n")
	return err
}

// csvSafe stops spreadsheet formula injection: a cell starting with = + - @ or a tab is prefixed
// with an apostrophe (OWASP CSV injection guidance).
func csvSafe(cells []string) []string {
	for i, c := range cells {
		if c != "" && strings.ContainsRune("=+-@\t\r", rune(c[0])) {
			cells[i] = "'" + c
		}
	}
	return cells
}
