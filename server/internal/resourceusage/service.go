// Package resourceusage projects accepted transport measurements for workspace
// users. It never debits quota or stores payloads, targets, or credentials.
package resourceusage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

const MaxWindow = 90 * 24 * time.Hour
const MaxResources = 200
const MaxPaths = 32

var ErrInvalid = errors.New("invalid usage window or filter")

type Query struct {
	ActorAccountID                          string
	Start, End                              time.Time
	ResourceID, ResourceKind, Mode, Hosting string
	RouteID, MachineID                      *string
	Limit, Offset, PathLimit, PathOffset    int
}
type Pagination struct {
	Limit      int   `json:"limit"`
	Offset     int   `json:"offset"`
	Total      int64 `json:"total"`
	NextOffset *int  `json:"next_offset"`
}

func page(limit, offset int, total int64) Pagination {
	p := Pagination{Limit: limit, Offset: offset, Total: total}
	if int64(offset)+int64(limit) < total {
		n := offset + limit
		p.NextOffset = &n
	}
	return p
}

type Bytes struct {
	Upload   int64 `json:"upload_bytes"`
	Download int64 `json:"download_bytes"`
}
type Path struct {
	Mode     string `json:"mode"`
	Hosting  string `json:"hosting"`
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	Bytes
}
type Resource struct {
	Kind        string `json:"resource_kind"`
	ID          string `json:"resource_id"`
	RouteID     string `json:"resource_route_id,omitempty"`
	Name        string `json:"resource_name"`
	MachineID   string `json:"machine_id"`
	MachineName string `json:"machine_name"`
	Bytes
	Paths          []Path     `json:"paths"`
	PathPagination Pagination `json:"path_pagination"`
}
type PathTotal struct {
	Mode    string `json:"mode"`
	Hosting string `json:"hosting"`
	Bytes
}
type Totals struct {
	Bytes
	ByPath []PathTotal `json:"by_path"`
}
type Point struct {
	Start time.Time `json:"start"`
	Totals
}
type Coverage struct {
	Edge   bool   `json:"edge"`
	Native bool   `json:"native"`
	Note   string `json:"note"`
}
type Result struct {
	Start      time.Time  `json:"start"`
	End        time.Time  `json:"end"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Coverage   Coverage   `json:"coverage"`
	Totals     Totals     `json:"totals"`
	Resources  []Resource `json:"resources"`
	Series     []Point    `json:"series"`
	Pagination Pagination `json:"pagination"`
}

// BandwidthDebiter charges hosted relay bytes to the workspace allowance inside the
// transaction that records them; billing.WorkspaceService is the owner.
type BandwidthDebiter interface {
	DebitWorkspaceBandwidthTx(ctx context.Context, tx *db.Tx, workspaceID string, bytes int64, at time.Time) (granted int64, exhausted bool, err error)
}

type Service struct {
	Store *db.DB
	Now   func() time.Time
	// Debiter is optional: without it native usage is history only.
	Debiter BandwidthDebiter
}

func New(store *db.DB) *Service { return &Service{Store: store} }
func (q Query) Validate(now time.Time) error {
	if len(q.ActorAccountID) > 256 || strings.TrimSpace(q.ActorAccountID) != q.ActorAccountID || !utf8.ValidString(q.ActorAccountID) {
		return ErrInvalid
	}
	if q.Start.IsZero() || !q.End.After(q.Start) || q.End.Sub(q.Start) > MaxWindow || q.End.After(now.Add(time.Minute)) || len(q.ResourceID) > 256 {
		return ErrInvalid
	}
	if q.Limit < 0 || q.Limit > MaxResources || q.Offset < 0 || q.Offset > 2147483647 || q.PathLimit < 0 || q.PathLimit > MaxPaths || q.PathOffset < 0 || q.PathOffset > 2147483647 {
		return ErrInvalid
	}
	for _, v := range []string{q.ResourceID, q.ResourceKind} {
		if len(v) > 256 || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return ErrInvalid
		}
	}
	for _, v := range []*string{q.RouteID, q.MachineID} {
		if v != nil && (len(*v) > 256 || !utf8.ValidString(*v) || strings.ContainsRune(*v, 0)) {
			return ErrInvalid
		}
	}
	switch q.Mode {
	case "", "direct", "relay", "edge", "unknown":
	default:
		return ErrInvalid
	}
	switch q.Hosting {
	case "", "managed", "self_hosted", "none", "unknown":
	default:
		return ErrInvalid
	}
	return nil
}

const predicate = ` e.workspace_id=$1 AND e.interval_end>=$2 AND e.interval_end<$3 AND ($4='' OR e.resource_id=$4) AND ($5='' OR e.mode=$5) AND ($6='' OR e.hosting=$6) AND ($7='' OR e.resource_kind=$7) AND (NOT $8::boolean OR e.resource_route_id=$9) AND (NOT $10::boolean OR e.machine_id=$11) AND ($12='' OR e.actor_account_id=$12) `

func (s *Service) Get(ctx context.Context, account string, scope workspace.Scope, q Query) (Result, error) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	out := Result{Start: q.Start.UTC(), End: q.End.UTC(), Resources: []Resource{}, Series: []Point{}, Coverage: Coverage{Edge: true, Native: true, Note: "History covers the past 90 days. Native traffic appears after a machine is updated; some earlier traffic may be missing. Traffic is grouped by the time each report ended."}}
	if q.ActorAccountID != "" {
		out.Coverage.Note += " Member totals include only traffic with a retained actor."
	}
	if err := q.Validate(now); err != nil {
		return out, err
	}
	ws := "personal:" + account
	if scope.TeamID != "" {
		ws = "team:" + scope.TeamID
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.PathLimit == 0 {
		q.PathLimit = MaxPaths
	}
	route, machine := "", ""
	if q.RouteID != nil {
		route = *q.RouteID
	}
	if q.MachineID != nil {
		machine = *q.MachineID
	}
	args := []any{ws, q.Start, q.End, q.ResourceID, q.Mode, q.Hosting, q.ResourceKind, q.RouteID != nil, route, q.MachineID != nil, machine, q.ActorAccountID}
	err := s.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, account, scope); err != nil {
			return err
		}
		var reported, expected int64
		var unrecorded, expired, rejected int64
		var stale, incomplete bool
		if err := tx.QueryRow(ctx, `SELECT count(status.machine_id),count(machine.id),
 coalesce(sum(status.unrecorded_bytes),0)::bigint,
 coalesce(sum(status.expired_bytes),0)::bigint,
 coalesce(sum(status.rejected_reports),0)::bigint,
 coalesce(bool_or(status.last_seen<$2),false),
 coalesce(bool_or(status.recording_incomplete),false)
 FROM user_machines machine LEFT JOIN native_usage_reporting_status status
 ON status.machine_id=machine.id AND status.installation_generation=machine.installation_generation
 AND status.workspace_id=$1
 WHERE coalesce('team:'||machine.owner_team_id,'personal:'||machine.user_id)=$1
 AND machine.deleted_at IS NULL AND machine.revoked_at IS NULL`, ws, now.Add(-time.Minute)).Scan(&reported, &expected, &unrecorded, &expired, &rejected, &stale, &incomplete); err != nil {
			return err
		}
		out.Coverage.Edge = !q.Start.Before(now.Add(-MaxWindow))
		out.Coverage.Native = !q.Start.Before(now.Add(-MaxWindow)) && reported > 0 && reported >= expected && unrecorded == 0 && expired == 0 && rejected == 0 && !stale && !incomplete
		if !out.Coverage.Native {
			out.Coverage.Note += fmt.Sprintf(" Reporting %d of %d machines; %d bytes were not recorded and %d bytes expired before reporting.", reported, expected, unrecorded, expired)
			if stale {
				out.Coverage.Note += " Some machines have not reported recently."
			}
			if incomplete {
				out.Coverage.Note += " Recording was interrupted; some traffic may be missing."
			}
			if rejected > 0 {
				out.Coverage.Note += " Some traffic reports could not be accepted."
			}
		}

		var stamp sql.NullTime
		if err := tx.QueryRow(ctx, `SELECT greatest((SELECT max(last_seen) FROM native_usage_reporting_status WHERE workspace_id=$1),(SELECT max(interval_end) FROM resource_bandwidth_events e WHERE workspace_id=$1))`, ws).Scan(&stamp); err != nil {
			return err
		}
		if stamp.Valid {
			out.UpdatedAt = stamp.Time.UTC()
		}
		if err := aggregateUsage(ctx, tx, args, &out); err != nil {
			return err
		}
		var total int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM resource_bandwidth_events e WHERE `+predicate+` GROUP BY resource_kind,resource_id,resource_route_id,machine_id) groups`, args...).Scan(&total); err != nil {
			return err
		}
		out.Pagination = page(q.Limit, q.Offset, total)
		detailArgs := append(append([]any{}, args...), q.Limit, q.Offset, q.PathLimit, q.PathOffset)
		rows, err := tx.Query(ctx, `WITH selected AS (
          SELECT resource_kind,resource_id,resource_route_id,machine_id,max(resource_name) resource_name,max(machine_name) machine_name,sum(upload_bytes)::bigint upload,sum(download_bytes)::bigint download FROM resource_bandwidth_events e WHERE `+predicate+`
          GROUP BY resource_kind,resource_id,resource_route_id,machine_id ORDER BY resource_kind,resource_id,resource_route_id,machine_id LIMIT $13 OFFSET $14
        ) SELECT r.resource_kind,r.resource_id,r.resource_route_id,r.resource_name,r.machine_id,r.machine_name,r.upload,r.download,c.total,p.mode,p.hosting,p.node_id,p.node_name,p.upload,p.download
        FROM selected r CROSS JOIN LATERAL (
          SELECT count(*) total FROM (SELECT 1 FROM resource_bandwidth_events e WHERE `+predicate+` AND e.resource_kind=r.resource_kind AND e.resource_id=r.resource_id AND e.resource_route_id=r.resource_route_id AND e.machine_id=r.machine_id GROUP BY mode,hosting,node_id) paths
        ) c LEFT JOIN LATERAL (
          SELECT mode,hosting,node_id,max(node_name) node_name,sum(upload_bytes)::bigint upload,sum(download_bytes)::bigint download FROM resource_bandwidth_events e WHERE `+predicate+` AND e.resource_kind=r.resource_kind AND e.resource_id=r.resource_id AND e.resource_route_id=r.resource_route_id AND e.machine_id=r.machine_id
          GROUP BY mode,hosting,node_id ORDER BY mode,hosting,node_id LIMIT $15 OFFSET $16
        ) p ON true ORDER BY r.resource_kind,r.resource_id,r.resource_route_id,r.machine_id,p.mode,p.hosting,p.node_id`, detailArgs...)
		if err != nil {
			return err
		}
		indexes := map[string]int{}
		for rows.Next() {
			var r Resource
			var totalPaths int64
			var mode, hosting, node, name sql.NullString
			var upload, download sql.NullInt64
			if err := rows.Scan(&r.Kind, &r.ID, &r.RouteID, &r.Name, &r.MachineID, &r.MachineName, &r.Upload, &r.Download, &totalPaths, &mode, &hosting, &node, &name, &upload, &download); err != nil {
				rows.Close()
				return err
			}
			key := r.Kind + "\x00" + r.ID + "\x00" + r.RouteID + "\x00" + r.MachineID
			index, ok := indexes[key]
			if !ok {
				index = len(out.Resources)
				indexes[key] = index
				r.Paths = []Path{}
				r.PathPagination = page(q.PathLimit, q.PathOffset, totalPaths)
				out.Resources = append(out.Resources, r)
			}
			if mode.Valid {
				out.Resources[index].Paths = append(out.Resources[index].Paths, Path{Mode: mode.String, Hosting: hosting.String, NodeID: node.String, NodeName: name.String, Bytes: Bytes{Upload: upload.Int64, Download: download.Int64}})
			}
		}
		err = rows.Err()
		rows.Close()
		return err
	})
	return out, err
}
