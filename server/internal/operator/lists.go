package operator

import (
	"context"
	"database/sql"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// totalCap bounds every list total: past it the panel shows "100000+" instead of the query scanning a whole table.
const totalCap = 100000

// Counted is embedded in every list page so the panel can show "a–b of N" and real page controls.
type Counted struct {
	Total       int64 `json:"total"`
	TotalCapped bool  `json:"total_capped,omitempty"`
}

// countRows counts the rows `from` (a FROM clause with its WHERE) returns, stopping at totalCap.
func countRows(ctx context.Context, tx *db.Tx, from string, args ...any) (Counted, error) {
	var n int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM `+from+` LIMIT `+strconv.Itoa(totalCap+1)+`) c`, args...).Scan(&n); err != nil {
		return Counted{}, err
	}
	if n > totalCap {
		return Counted{Total: totalCap, TotalCapped: true}, nil
	}
	return Counted{Total: n}, nil
}

// parseFilter reads the url-encoded filter target the handler builds from the query string. Unknown keys and
// oversized or control-character values are refused, so a filter never reaches SQL unchecked.
func parseFilter(target string, keys ...string) (map[string]string, error) {
	values, err := url.ParseQuery(target)
	if err != nil {
		return nil, ErrInvalid
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		known := false
		for _, key := range keys {
			known = known || k == key
		}
		if !known || len(v) != 1 || len(v[0]) > 128 || strings.ContainsFunc(v[0], func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return nil, ErrInvalid
		}
		out[k] = strings.TrimSpace(v[0])
	}
	return out, nil
}

// contains turns a search word into an ILIKE pattern that treats % and _ literally (use with ESCAPE '\').
func contains(q string) string {
	if q == "" {
		return ""
	}
	return `%` + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + `%`
}

func oneOf(value string, allowed ...string) bool {
	if value == "" {
		return true
	}
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

type Machine struct {
	ID                string     `json:"id"`
	Alias             string     `json:"alias"`
	AccountID         string     `json:"account_id"`
	TeamID            string     `json:"team_id,omitempty"`
	Platform          string     `json:"platform"`
	Architecture      string     `json:"architecture"`
	State             string     `json:"state"`
	Ready             bool       `json:"ready"`
	Stale             bool       `json:"stale"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty"`
	EnrolledAt        *time.Time `json:"enrolled_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	DeletedAt         *time.Time `json:"deleted_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	HostServiceVer    string     `json:"host_service_version,omitempty"`
	ConnectorState    string     `json:"connector_state"`
	UpdateHealth      string     `json:"update_health"`
	Capabilities      []string   `json:"observed_capabilities"`
	InstallGeneration int64      `json:"installation_generation"`
}

type MachinePage struct {
	Counted
	Machines  []Machine `json:"machines"`
	NextAfter string    `json:"next_after,omitempty"`
}

const machineColumns = `m.id,m.alias,m.user_id,coalesce(m.owner_team_id,''),m.platform,m.architecture,m.state,
 (m.online AND m.state='online' AND m.last_seen_at>now()-($1*interval '1 microsecond')),
 (m.last_seen_at IS NULL OR m.last_seen_at<=now()-($1*interval '1 microsecond')),
 m.last_seen_at,m.enrolled_at,m.revoked_at,m.deleted_at,m.created_at,coalesce(m.host_service_version,''),m.connector_state,m.update_health,m.observed_capabilities,m.installation_generation`

type rowScanner interface{ Scan(...any) error }

func scanMachine(row rowScanner) (Machine, error) {
	var v Machine
	var seen, enrolled, revoked, deleted sql.NullTime
	err := row.Scan(&v.ID, &v.Alias, &v.AccountID, &v.TeamID, &v.Platform, &v.Architecture, &v.State, &v.Ready, &v.Stale,
		&seen, &enrolled, &revoked, &deleted, &v.CreatedAt, &v.HostServiceVer, &v.ConnectorState, &v.UpdateHealth, &v.Capabilities, &v.InstallGeneration)
	for _, t := range []struct {
		from sql.NullTime
		to   **time.Time
	}{{seen, &v.LastSeenAt}, {enrolled, &v.EnrolledAt}, {revoked, &v.RevokedAt}, {deleted, &v.DeletedAt}} {
		if t.from.Valid {
			ts := t.from.Time
			*t.to = &ts
		}
	}
	if v.Capabilities == nil {
		v.Capabilities = []string{}
	}
	return v, err
}

func (s *Service) readMachine(ctx context.Context, tx *db.Tx, id string) (Machine, error) {
	v, err := scanMachine(tx.QueryRow(ctx, `SELECT `+machineColumns+` FROM user_machines m WHERE m.id=$2`, s.hostStaleAfter.Microseconds(), id))
	return v, scanErr(err)
}

// readMachines filters: q (id, alias, owner account or team contains), account and team (exact owner), state,
// platform, stale=true|false, deleted=true to list removed machines. Removed machines are hidden by default.
func (s *Service) readMachines(ctx context.Context, tx *db.Tx, target, after string, limit int) (_ MachinePage, resultErr error) {
	f, err := parseFilter(target, "q", "account", "team", "state", "platform", "stale", "deleted")
	if err != nil || !oneOf(f["state"], "pending", "online", "offline", "disconnected", "revoked", "deleted") ||
		!oneOf(f["platform"], "linux", "darwin", "windows") || !oneOf(f["stale"], "true", "false") || !oneOf(f["deleted"], "true") {
		return MachinePage{}, ErrInvalid
	}
	where := ` user_machines m WHERE ($2='' OR m.id ILIKE $2 ESCAPE '\' OR m.alias ILIKE $2 ESCAPE '\' OR m.user_id ILIKE $2 ESCAPE '\' OR m.owner_team_id ILIKE $2 ESCAPE '\')
 AND ($3='' OR m.state=$3) AND ($4='' OR m.platform=$4)
 AND ($5='' OR (m.last_seen_at IS NULL OR m.last_seen_at<=now()-($1*interval '1 microsecond'))=($5='true'))
 AND (($6='true') OR m.deleted_at IS NULL) AND ($7='' OR m.user_id=$7) AND ($8='' OR m.owner_team_id=$8)`
	args := []any{s.hostStaleAfter.Microseconds(), contains(f["q"]), f["state"], f["platform"], f["stale"], f["deleted"], f["account"], f["team"]}
	page := MachinePage{Machines: make([]Machine, 0, limit)}
	if page.Counted, err = countRows(ctx, tx, where, args...); err != nil {
		return page, err
	}
	rows, err := tx.Query(ctx, `SELECT `+machineColumns+` FROM`+where+` AND m.id>$9 ORDER BY m.id LIMIT $10`, append(args, after, limit+1)...)
	if err != nil {
		return page, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	for rows.Next() {
		v, err := scanMachine(rows)
		if err != nil {
			return page, err
		}
		if len(page.Machines) == limit {
			page.NextAfter = page.Machines[len(page.Machines)-1].ID
			break
		}
		page.Machines = append(page.Machines, v)
	}
	return page, rows.Err()
}

type AuditSearchPage struct {
	Counted
	Events    []AuditEvent `json:"events"`
	NextAfter string       `json:"next_after,omitempty"`
}

// readAuditEvents searches the whole audit log, newest first. Filters: actor, event_type, resource_type,
// resource_id, outcome, since, until (RFC3339). The cursor is the event's sequence number.
func readAuditEvents(ctx context.Context, tx *db.Tx, target, after string, limit int) (_ AuditSearchPage, resultErr error) {
	f, err := parseFilter(target, "actor", "event_type", "resource_type", "resource_id", "outcome", "since", "until")
	if err != nil || !oneOf(f["outcome"], "unchanged", "changed", "uncertain") {
		return AuditSearchPage{}, ErrInvalid
	}
	times := [2]time.Time{}
	for i, key := range []string{"since", "until"} {
		if f[key] == "" {
			continue
		}
		if times[i], err = time.Parse(time.RFC3339, f[key]); err != nil {
			return AuditSearchPage{}, ErrInvalid
		}
	}
	cursor := int64(1<<62 - 1)
	if after != "" {
		if cursor, err = strconv.ParseInt(after, 10, 64); err != nil || cursor < 1 {
			return AuditSearchPage{}, ErrInvalid
		}
	}
	where := ` audit_events WHERE ($1='' OR actor_user_id=$1) AND ($2='' OR event_type=$2) AND ($3='' OR resource_type=$3)
 AND ($4='' OR resource_id=$4) AND ($5='' OR outcome=$5) AND ($6::timestamptz IS NULL OR created_at>=$6) AND ($7::timestamptz IS NULL OR created_at<$7)`
	args := []any{f["actor"], f["event_type"], f["resource_type"], f["resource_id"], f["outcome"], nullTime(times[0]), nullTime(times[1])}
	page := AuditSearchPage{Events: make([]AuditEvent, 0, limit)}
	if page.Counted, err = countRows(ctx, tx, where, args...); err != nil {
		return page, err
	}
	rows, err := tx.Query(ctx, `SELECT id,coalesce(actor_user_id,''),actor_type,event_type,resource_type,resource_id,outcome,coalesce(request_id,''),created_at,cursor_sequence FROM`+where+` AND cursor_sequence<$8 ORDER BY cursor_sequence DESC LIMIT $9`, append(args, cursor, limit+1)...)
	if err != nil {
		return page, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	var last int64
	for rows.Next() {
		var v AuditEvent
		var seq int64
		if err := rows.Scan(&v.ID, &v.ActorUserID, &v.ActorType, &v.EventType, &v.ResourceType, &v.ResourceID, &v.Outcome, &v.RequestID, &v.CreatedAt, &seq); err != nil {
			return page, err
		}
		if len(page.Events) == limit {
			page.NextAfter = strconv.FormatInt(last, 10)
			break
		}
		last = seq
		page.Events = append(page.Events, v)
	}
	return page, rows.Err()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
