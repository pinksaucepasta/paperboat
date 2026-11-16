// Package accountingauthority preserves only the payer binding needed to settle
// delayed usage. These records never authorize access to a resource.
package accountingauthority

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// DeliveryWindow permits an edge offline for a weekend to deliver its durable
// queue, with additional recovery time. The v1 wire contract uses the same seven
// day bound in paperboat-edge/internal/usage/policy.go.
const DeliveryWindow = 7 * 24 * time.Hour

// MaximumPerNode bounds the bindings a node holds admitted at once (the edge's 4096-route registry).
// Admission fails closed at capacity. Records past their admission stay until retain_until so a delayed
// report is still billed, but only pruning by time removes them: they do not count, or a node that
// admitted one short-lived preview a minute for DeliveryWindow+ refused every new one (2026-11-12, prod previews 404).
const MaximumPerNode = 4096

var ErrQuotaExhausted = errors.New("workspace bandwidth allowance is exhausted")

// QuotaExhaustedError is ErrQuotaExhausted with the end of the allowance period it applies to, so
// the edge can tell a visitor when the address resumes.
type QuotaExhaustedError struct{ ResetsAt time.Time }

func (e *QuotaExhaustedError) Error() string { return ErrQuotaExhausted.Error() }
func (e *QuotaExhaustedError) Unwrap() error { return ErrQuotaExhausted }

// quotaExhaustedTx is the exhaustion answer with the current period's end, read from the period row
// the quota check just found or created.
func quotaExhaustedTx(ctx context.Context, tx *db.Tx, workspace string, at time.Time) error {
	var end sql.NullTime
	if err := tx.QueryRow(ctx, `SELECT max(period_end) FROM billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=$2 AND period_end>$2`, workspace, at).Scan(&end); err != nil {
		return err
	}
	// No covering row: the reset time is unknown, the refusal stands.
	return &QuotaExhaustedError{ResetsAt: end.Time.UTC()}
}

var ErrUnavailable = errors.New("usage accounting authority is unavailable; retry after retained usage expires")

type Binding struct {
	ID, Node, Environment, Route, User, Team string
	Revision                                 int64
	IssuedAt, ExpiresAt                      time.Time
}

type QuotaChecker interface {
	CheckWorkspaceBandwidthTx(context.Context, *db.Tx, string, time.Time) (bool, error)
}

// Record admits b against its workspace's bandwidth allowance, judged by policy: the one authoritative check, which
// counts budget segments and top-ups. Without a policy admission cannot be judged, so it fails closed.
func Record(ctx context.Context, store *db.DB, b Binding, policy QuotaChecker) error {
	return RecordMany(ctx, store, []Binding{b}, policy)[0]
}

// nodeLocks stripes the per-node Record lock (a one-slot channel, so a cancelled caller stops waiting); nodes that share
// a stripe only queue behind each other in this process.
var nodeLocks = func() (locks [64]chan struct{}) {
	for i := range locks {
		locks[i] = make(chan struct{}, 1)
	}
	return locks
}()

func nodeStripe(node string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(node))
	return int(h.Sum32() % uint32(len(nodeLocks)))
}
