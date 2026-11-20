// Package hosthealth keeps the latest host-capacity and canary report that the production canary runner pushed, for the admin
// panel. The sampler sits outside the production hosts, so it pushes with a dedicated credential, like backupreport. Prior art:
// Teleport keeps each resource's health on the auth service for its UI (~/ref/teleport/lib/services/health_check_config.go).
package hosthealth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var ErrInvalid = errors.New("invalid host health report")

// maxPayload bounds one report: a few hosts with a handful of resources each, or one canary run.
const maxPayload = 32 << 10

// Report is one source's whole latest state. Sources are "hosts" (per host resource percentages and levels) and "canary".
type Report struct {
	Source  string          `json:"source"`
	Payload json.RawMessage `json:"payload"`
}

type Entry struct {
	Source     string          `json:"source"`
	Payload    json.RawMessage `json:"payload"`
	ReportedAt time.Time       `json:"reported_at"`
}

type Service struct {
	store *db.DB
	token string
}

func NewService(store *db.DB, token string) *Service { return &Service{store: store, token: token} }

// Authenticate compares in constant time. Without a configured token nothing authenticates.
func (s *Service) Authenticate(token string) bool {
	if s == nil || s.token == "" || len(token) > 512 {
		return false
	}
	a, b := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(s.token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (s *Service) Ingest(ctx context.Context, r Report) error {
	var object map[string]json.RawMessage
	if (r.Source != "hosts" && r.Source != "canary") || len(r.Payload) == 0 || len(r.Payload) > maxPayload || json.Unmarshal(r.Payload, &object) != nil || object == nil {
		return ErrInvalid
	}
	_, err := s.store.Pool().Exec(ctx, `INSERT INTO host_health_reports(source,payload,reported_at) VALUES($1,$2,now())
ON CONFLICT(source) DO UPDATE SET payload=EXCLUDED.payload,reported_at=EXCLUDED.reported_at`, r.Source, []byte(r.Payload))
	return err
}

func (s *Service) Snapshot(ctx context.Context) ([]Entry, error) {
	rows, err := s.store.Pool().Query(ctx, `SELECT source,payload,reported_at FROM host_health_reports ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Source, &e.Payload, &e.ReportedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
