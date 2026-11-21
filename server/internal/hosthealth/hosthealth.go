// Package hosthealth keeps the latest host-capacity and canary report that the production canary runner pushed, for the admin
// panel, and each edge's certificate renewal check from that edge's heartbeat. Operator-side reporters push through the
// operator API; edges never write here. Prior art:
// Teleport keeps each resource's health on the auth service for its UI (~/ref/teleport/lib/services/health_check_config.go).
package hosthealth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var ErrInvalid = errors.New("invalid host health report")

// maxPayload bounds one report: a few hosts with a handful of resources each, or one canary run.
const maxPayload = 32 << 10

// maxEdges bounds the edge certificate entries the snapshot lists; we run a handful of edges.
const maxEdges = 64

// edgeCertificateStaleAfter: the renewal timer checks every six hours, so a day without a check is four missed runs.
const edgeCertificateStaleAfter = 86400

// Report is one operator-side source's whole latest state: "hosts" (per host resource percentages and levels),
// "canary" or "deploy" (the gated deploy's last outcome). Operators push it through the operator API.
type Report struct {
	Source  string          `json:"source"`
	Payload json.RawMessage `json:"payload"`
}

type Entry struct {
	Source     string          `json:"source"`
	Payload    json.RawMessage `json:"payload"`
	ReportedAt time.Time       `json:"reported_at"`
}

type Service struct{ store *db.DB }

func NewService(store *db.DB) *Service { return &Service{store: store} }

func (s *Service) Ingest(ctx context.Context, r Report) error {
	var object map[string]json.RawMessage
	switch r.Source {
	case "hosts", "canary", "deploy":
	default:
		return ErrInvalid
	}
	if len(r.Payload) == 0 || len(r.Payload) > maxPayload || json.Unmarshal(r.Payload, &object) != nil || object == nil {
		return ErrInvalid
	}
	_, err := s.store.Pool().Exec(ctx, `INSERT INTO host_health_reports(source,payload,reported_at) VALUES($1,$2,now())
ON CONFLICT(source) DO UPDATE SET payload=EXCLUDED.payload,reported_at=EXCLUDED.reported_at`, r.Source, []byte(r.Payload))
	return err
}

// Snapshot lists the operator reports, then one "edge-certificate:node-<id>" entry per edge whose heartbeats carried a
// certificate renewal check. That entry comes only from the node's own authenticated heartbeat and is kept until the node
// reports another check (also while it is offline), so only that node raises or clears it; its time is the check's, so a
// silent edge goes stale. Failing edges sort first, so the bound never hides one behind healthy edges.
func (s *Service) Snapshot(ctx context.Context) ([]Entry, error) {
	out := []Entry{}
	rows, err := s.store.Pool().Query(ctx, `SELECT source,payload,reported_at FROM host_health_reports ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Source, &e.Payload, &e.ReportedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	edges, err := s.store.Pool().Query(ctx, `SELECT id, observation->'certificate_renewal' FROM control_tunnel_nodes
WHERE observation ? 'certificate_renewal' AND state <> 'retired'
ORDER BY coalesce((observation->'certificate_renewal'->'ok') = 'true'::jsonb, false), id LIMIT $1`, maxEdges)
	if err != nil {
		return nil, err
	}
	defer edges.Close()
	for edges.Next() {
		var node string
		var raw []byte
		if err := edges.Scan(&node, &raw); err != nil {
			return nil, err
		}
		// One node's stored value never breaks the snapshot: an unreadable one is shown as a failing check.
		var check struct {
			OK        *bool     `json:"ok"`
			Error     string    `json:"error"`
			CheckedAt time.Time `json:"checked_at"`
		}
		payload := map[string]any{"ok": false, "error": "the reported certificate check is unreadable", "stale_after_seconds": edgeCertificateStaleAfter}
		at := time.Time{}
		if json.Unmarshal(raw, &check) == nil && check.OK != nil && !check.CheckedAt.IsZero() {
			payload["ok"], payload["error"], at = *check.OK, check.Error, check.CheckedAt
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{Source: "edge-certificate:node-" + node, Payload: encoded, ReportedAt: at})
	}
	return out, edges.Err()
}
