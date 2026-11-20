// Package machinelatency stores the connect time each machine measured to the edges and relays it
// can use. They feed two things: the console and CLI show them, and the edgeset package ranks the
// edges for a workspace's default pair by the best fresh edge RTT across its machines. They never
// pick a path per connection, never override a pair chosen in the console, and never confer access. A machine's edge numbers arrive on its runtime attachment (the server
// accepts them only for candidates it issued); its relay numbers arrive from its signed-in daemon.
package machinelatency

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// MaxItems bounds one report and one listing.
const MaxItems = 64

// Fresh is how long a measurement is shown; an older one is stale and hidden.
const Fresh = time.Hour

// Refresh is how old a stored sample may get before a similar one (within 5 ms or 20 %) replaces it.
const Refresh = 10 * time.Minute

var ErrInvalid = errors.New("invalid machine latency report")

// Item is the best measurement a machine has to one region and role: its edges (or relays) in that region, the nearest
// of them. Users see regions only, so no node id appears; Name is Paperboat's display name for a managed region and the
// region id for a customer's own (SelfHosted).
type Item struct {
	Region     string `json:"region"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	RTTMillis  int    `json:"rtt_ms"`
	SelfHosted bool   `json:"self_hosted,omitempty"` // run by a customer; Paperboat's own pbh nodes have global scope and are not
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
		// Every attachment repeats its samples; a similar one is kept until it is Refresh old.
		if _, err := tx.Exec(ctx, `INSERT INTO machine_node_latency(machine_id,node_id,role,rtt_ms,measured_at) VALUES($1,$2,'edge',$3,$4)
ON CONFLICT(machine_id,node_id) DO UPDATE SET role='edge',rtt_ms=EXCLUDED.rtt_ms,measured_at=EXCLUDED.measured_at
WHERE machine_node_latency.role<>'edge' OR machine_node_latency.measured_at<$5
 OR (abs(machine_node_latency.rtt_ms-EXCLUDED.rtt_ms)>=5 AND abs(machine_node_latency.rtt_ms-EXCLUDED.rtt_ms)*5>=greatest(machine_node_latency.rtt_ms,1))`, machineID, sample.NodeID, sample.RTTMilliseconds, now, now.Add(-Refresh)); err != nil {
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

// List returns a machine's fresh measurements as the best RTT per (region, role), edges first, fastest first inside
// each role.
func List(ctx context.Context, database *sql.DB, machineID string, now time.Time) ([]Item, error) {
	rows, err := database.QueryContext(ctx, `SELECT n.region,
  CASE WHEN bool_or(EXISTS(SELECT 1 FROM selfhost_installations i WHERE i.node_id=n.id AND i.scope_kind<>'global')) THEN n.region ELSE coalesce(max(r.display_name),n.region) END,
  l.role,min(l.rtt_ms),bool_or(EXISTS(SELECT 1 FROM selfhost_installations i WHERE i.node_id=n.id AND i.scope_kind<>'global'))
FROM machine_node_latency l JOIN control_tunnel_nodes n ON n.id=l.node_id AND n.region IS NOT NULL
LEFT JOIN operator_regions r ON r.id=n.region
WHERE l.machine_id=$1 AND l.measured_at>$2::timestamptz-make_interval(secs=>$3::float8)
GROUP BY n.region,l.role ORDER BY l.role,min(l.rtt_ms),n.region LIMIT $4`, machineID, now, Fresh.Seconds(), MaxItems)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.Region, &item.Name, &item.Role, &item.RTTMillis, &item.SelfHosted); err != nil {
			return nil, err
		}
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

// Best is the lowest RTT any of an account's machines measured to one node within Fresh, and when.
type Best struct {
	RTTMillis  int
	MeasuredAt time.Time
}

// BestForAccount returns Best for each of nodeIDs (a page of at most 100) that one of the account's
// machines measured in the last Fresh; nodes without a fresh sample are absent. One bounded query.
func BestForAccount(ctx context.Context, database *sql.DB, accountID string, nodeIDs []string, now time.Time) (map[string]Best, error) {
	out := map[string]Best{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	rows, err := database.QueryContext(ctx, `SELECT DISTINCT ON (l.node_id) l.node_id,l.rtt_ms,l.measured_at
FROM machine_node_latency l JOIN user_machines m ON m.id=l.machine_id
WHERE m.user_id=$1 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND l.node_id=ANY($2::text[])
 AND l.measured_at>$3::timestamptz-make_interval(secs=>$4::float8)
ORDER BY l.node_id,l.rtt_ms,l.measured_at DESC`, accountID, nodeIDs, now, Fresh.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var best Best
		if err := rows.Scan(&id, &best.RTTMillis, &best.MeasuredAt); err != nil {
			return nil, err
		}
		best.MeasuredAt = best.MeasuredAt.UTC()
		out[id] = best
	}
	return out, rows.Err()
}
