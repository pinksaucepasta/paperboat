// Package machinelatency stores the connect time each machine measured to the edges and relays it
// can use. The numbers are display data for the console and CLI: they never select a path and
// never confer access. A machine's edge numbers arrive on its runtime attachment (the server
// accepts them only for candidates it issued); its relay numbers arrive from its signed-in daemon.
package machinelatency

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// MaxItems bounds one report and one listing.
const MaxItems = 64

// Fresh is how long a measurement is shown; an older one is stale and hidden.
const Fresh = time.Hour

var ErrInvalid = errors.New("invalid machine latency report")

// Item is one listed measurement with the node's public inventory fields.
type Item struct {
	NodeID     string    `json:"node_id"`
	Role       string    `json:"role"`
	Region     string    `json:"region"`
	SelfHosted bool      `json:"self_hosted"` // run by a customer; Paperboat's own pbh nodes have global scope and are not
	RTTMillis  int       `json:"rtt_ms"`
	MeasuredAt time.Time `json:"measured_at"`
}

// RelaySample is one relay measurement reported by a machine's daemon.
type RelaySample struct {
	NodeID    string `json:"node_id"`
	RTTMillis int64  `json:"rtt_ms"`
}

func validRTT(value int64) bool { return value >= 0 && value <= runtimewire.MaximumLatencyMilliseconds }

// SaveEdgesTx records the edge measurements of one runtime attachment. Only a candidate the server
// issued in this same request is accepted; anything else is ignored rather than failing the attachment.
func SaveEdgesTx(ctx context.Context, tx *db.Tx, machineID string, samples []runtimewire.EdgeLatency, candidates []runtimewire.EdgeCandidate, now time.Time) error {
	if len(samples) > runtimewire.MaximumEdgeCandidates {
		return ErrInvalid
	}
	issued := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		issued[candidate.NodeID+"\x00"+candidate.ProcessEpoch] = true
	}
	for _, sample := range samples {
		if !issued[sample.NodeID+"\x00"+sample.ProcessEpoch] || !validRTT(sample.RTTMilliseconds) {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO machine_node_latency(machine_id,node_id,role,rtt_ms,measured_at) VALUES($1,$2,'edge',$3,$4)
ON CONFLICT(machine_id,node_id) DO UPDATE SET role='edge',rtt_ms=EXCLUDED.rtt_ms,measured_at=EXCLUDED.measured_at`, machineID, sample.NodeID, sample.RTTMilliseconds, now); err != nil {
			return err
		}
	}
	return nil
}

// SaveRelays records a daemon's relay measurements. Each node must be a relay the account may use.
func SaveRelays(ctx context.Context, database *sql.DB, accountID, machineID string, samples []RelaySample, now time.Time) error {
	if len(samples) == 0 || len(samples) > MaxItems {
		return ErrInvalid
	}
	seen := make(map[string]bool, len(samples))
	for _, sample := range samples {
		if sample.NodeID == "" || len(sample.NodeID) > 128 || seen[sample.NodeID] || !validRTT(sample.RTTMillis) {
			return ErrInvalid
		}
		seen[sample.NodeID] = true
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, sample := range samples {
		result, err := tx.ExecContext(ctx, `INSERT INTO machine_node_latency(machine_id,node_id,role,rtt_ms,measured_at)
SELECT $1,n.id,'relay',$4,$5 FROM control_tunnel_nodes n
WHERE n.id=$2 AND n.roles @> ARRAY['relay']::text[] AND (n.allowed_account_ids IS NULL OR $3=ANY(n.allowed_account_ids))
ON CONFLICT(machine_id,node_id) DO UPDATE SET role='relay',rtt_ms=EXCLUDED.rtt_ms,measured_at=EXCLUDED.measured_at`, machineID, sample.NodeID, accountID, sample.RTTMillis, now)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows == 0 {
			return ErrInvalid
		}
	}
	return tx.Commit()
}

// List returns a machine's fresh measurements, edges first, fastest first inside each role.
func List(ctx context.Context, database *sql.DB, machineID string, now time.Time) ([]Item, error) {
	rows, err := database.QueryContext(ctx, `SELECT l.node_id,l.role,coalesce(n.region,''),
  EXISTS(SELECT 1 FROM selfhost_installations i WHERE i.node_id=l.node_id AND i.scope_kind<>'global'),l.rtt_ms,l.measured_at
FROM machine_node_latency l JOIN control_tunnel_nodes n ON n.id=l.node_id
WHERE l.machine_id=$1 AND l.measured_at>$2::timestamptz-make_interval(secs=>$3::float8)
ORDER BY l.role,l.rtt_ms,l.node_id LIMIT $4`, machineID, now, Fresh.Seconds(), MaxItems)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.NodeID, &item.Role, &item.Region, &item.SelfHosted, &item.RTTMillis, &item.MeasuredAt); err != nil {
			return nil, err
		}
		item.MeasuredAt = item.MeasuredAt.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// Service is the HTTP-facing owner of machine latency.
type Service struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// SaveRelays records relay measurements for a machine the caller has already been authorized for.
func (s *Service) SaveRelays(ctx context.Context, accountID, machineID string, samples []RelaySample) error {
	return SaveRelays(ctx, s.DB, accountID, machineID, samples, s.now())
}

// List returns the machine's fresh measurements.
func (s *Service) List(ctx context.Context, machineID string) ([]Item, error) {
	return List(ctx, s.DB, machineID, s.now())
}
