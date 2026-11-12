package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
)

var (
	ErrInvalidUsageReport     = errors.New("invalid usage report")
	ErrUsageOperationConflict = errors.New("usage operation conflicts with an existing report")
)

type UsageReport struct {
	AuthorityID   string `json:"authority_id"`
	OperationID   string
	EdgeNodeID    string
	CounterEpoch  string
	EnvironmentID string
	RouteID       string
	RouteRevision int64
	Direction     string
	Bytes         int64
	IntervalStart time.Time
	IntervalEnd   time.Time
}

type UsageReceipt struct {
	QuotaExhausted bool
	Disposition    string
	OperationID    string
	DeltaBytes     int64
	ObservedBytes  int64
	Duplicate      bool
}

type ScopedBandwidthDebiter interface {
	DebitWorkspaceBandwidthTx(context.Context, *db.Tx, string, int64, time.Time) (int64, bool, error)
}

type BandwidthDebiter interface {
	DebitEnvironmentBandwidthTx(context.Context, *db.Tx, string, int64, time.Time) (granted int64, exhausted bool, err error)
}

// ReconcileUsage persists an edge report and advances its absolute counter in one
// short READ COMMITTED transaction. It returns the original result for an exact operation
// retry, and never decreases a counter for a lower or delayed observation.
func ReconcileUsage(ctx context.Context, store *db.DB, report UsageReport, now time.Time) (UsageReceipt, error) {
	return reconcileUsage(ctx, store, report, now, nil, nil)
}

func ReconcileUsageWithBandwidth(ctx context.Context, store *db.DB, report UsageReport, now time.Time, debiter BandwidthDebiter) (UsageReceipt, error) {
	return reconcileUsage(ctx, store, report, now, debiter, nil)
}

func reconcileUsage(ctx context.Context, store *db.DB, report UsageReport, now time.Time, debiter BandwidthDebiter, scoped ScopedBandwidthDebiter) (UsageReceipt, error) {
	if err := validateUsageReport(report); err != nil {
		return UsageReceipt{}, err
	}
	var result UsageReceipt
	// READ COMMITTED, not SERIALIZABLE: the rows every edge of one machine or workspace shares (bandwidth
	// period, topups) are updated by row-locked atomic statements, so concurrent reports queue on the row
	// lock and read its latest value instead of aborting each other with a serialization failure (8 edges
	// plus heartbeats exhausted ten serializable retries and answered 503). Duplicates are settled by the
	// receipt's unique operation_id; the counter row is created and locked before its delta is read.
	err := store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		existing, err := tx.Queries().GetControlUsageReceipt(ctx, report.OperationID)
		if err == nil {
			if !sameUsageReport(existing, report) {
				return ErrUsageOperationConflict
			}
			result = UsageReceipt{QuotaExhausted: existing.QuotaExhausted, Disposition: existing.Disposition, OperationID: report.OperationID, DeltaBytes: existing.DeltaBytes, ObservedBytes: existing.ObservedBytes, Duplicate: true}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("look up usage receipt: %w", err)
		}

		// The row must exist before it is locked: two concurrent first reports of one counter would otherwise
		// both read no row and both count the whole value as new.
		if _, err := tx.Exec(ctx, `INSERT INTO control_usage_counters (authority_id, edge_node_id, counter_epoch, environment_id, route_id, route_revision, direction, bytes, observed_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,0,$8) ON CONFLICT (edge_node_id, counter_epoch, environment_id, route_id, direction, authority_id) DO NOTHING`,
			report.AuthorityID, report.EdgeNodeID, report.CounterEpoch, report.EnvironmentID, report.RouteID, report.RouteRevision, report.Direction, report.IntervalEnd); err != nil {
			return fmt.Errorf("create usage counter: %w", err)
		}
		previous, err := tx.Queries().GetControlUsageCounterForUpdate(ctx, dbsqlc.GetControlUsageCounterForUpdateParams{
			EdgeNodeID: report.EdgeNodeID, CounterEpoch: report.CounterEpoch, EnvironmentID: report.EnvironmentID,
			RouteID: report.RouteID, Direction: report.Direction, AuthorityID: report.AuthorityID,
		})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock usage counter: %w", err)
		}
		var previousBytes int64
		if err == nil {
			previousBytes = previous.Bytes
		}
		delta := report.Bytes - previousBytes
		if delta < 0 {
			delta = 0
		}
		if _, err := tx.Queries().InsertControlUsageReceipt(ctx, dbsqlc.InsertControlUsageReceiptParams{
			OperationID: report.OperationID, EdgeNodeID: report.EdgeNodeID, CounterEpoch: report.CounterEpoch,
			EnvironmentID: report.EnvironmentID, RouteID: report.RouteID, RouteRevision: report.RouteRevision,
			Direction: report.Direction, AuthorityID: report.AuthorityID, ObservedBytes: report.Bytes, DeltaBytes: delta,
			IntervalStart: report.IntervalStart, IntervalEnd: report.IntervalEnd,
		}); err != nil {
			if errors.Is(err, sql.ErrNoRows) { // the same operation committed first: answer from its receipt
				stored, lookupErr := tx.Queries().GetControlUsageReceipt(ctx, report.OperationID)
				if lookupErr != nil {
					return fmt.Errorf("look up usage receipt: %w", lookupErr)
				}
				if !sameUsageReport(stored, report) {
					return ErrUsageOperationConflict
				}
				result = UsageReceipt{QuotaExhausted: stored.QuotaExhausted, Disposition: stored.Disposition, OperationID: report.OperationID, DeltaBytes: stored.DeltaBytes, ObservedBytes: stored.ObservedBytes, Duplicate: true}
				return nil
			}
			return fmt.Errorf("persist usage receipt: %w", err)
		}
		disposition := "accounted"
		quotaExhausted := false
		if scoped != nil && now.Sub(report.IntervalEnd) > accountingauthority.DeliveryWindow {
			disposition = "expired_not_billed"
			if _, err := tx.Exec(ctx, `UPDATE control_usage_receipts SET disposition=$2 WHERE operation_id=$1`, report.OperationID, disposition); err != nil {
				return err
			}
		} else if scoped != nil {
			var workspace string
			var actor sql.NullString
			if report.AuthorityID == "" || len(report.AuthorityID) > 512 || report.IntervalEnd.After(now) {
				return invalidUsage("usage.go:116")
			}
			if err := tx.QueryRow(ctx, `SELECT workspace_id,actor_account_id FROM usage_accounting_authorities WHERE authority_id=$1 AND edge_node_id=$2 AND environment_id=$3 AND route_id=$4 AND route_revision=$5 AND admitted_at<=$6 AND retain_until>$7`, report.AuthorityID, report.EdgeNodeID, report.EnvironmentID, report.RouteID, report.RouteRevision, report.IntervalEnd, now).Scan(&workspace, &actor); err != nil {
				return invalidUsage("usage.go:119")
			}
			if _, err := tx.Exec(ctx, `UPDATE control_usage_receipts SET workspace_id=$2,actor_account_id=$3 WHERE operation_id=$1`, report.OperationID, workspace, actor); err != nil {
				return err
			}
			if delta > 0 {
				_, exhausted, err := scoped.DebitWorkspaceBandwidthTx(ctx, tx, workspace, delta, report.IntervalEnd)
				if err != nil {
					return err
				}
				quotaExhausted = exhausted
				if _, err = tx.Exec(ctx, `UPDATE control_usage_receipts SET quota_exhausted=$2 WHERE operation_id=$1`, report.OperationID, exhausted); err != nil {
					return err
				}
			}
		}
		if debiter != nil && delta > 0 {
			_, _, err := debiter.DebitEnvironmentBandwidthTx(ctx, tx, report.EnvironmentID, delta, report.IntervalEnd)
			if err != nil {
				return fmt.Errorf("debit bandwidth: %w", err)
			}
		}
		if _, err := tx.Queries().UpsertControlUsageCounter(ctx, dbsqlc.UpsertControlUsageCounterParams{
			EdgeNodeID: report.EdgeNodeID, CounterEpoch: report.CounterEpoch, EnvironmentID: report.EnvironmentID,
			RouteID: report.RouteID, RouteRevision: report.RouteRevision, Direction: report.Direction, AuthorityID: report.AuthorityID,
			Bytes: report.Bytes, ObservedAt: report.IntervalEnd,
		}); err != nil {
			return fmt.Errorf("advance usage counter: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT paperboat.project_edge_bandwidth($1)`, report.OperationID); err != nil {
			return fmt.Errorf("record resource bandwidth: %w", err)
		}
		result = UsageReceipt{QuotaExhausted: quotaExhausted, Disposition: disposition, OperationID: report.OperationID, DeltaBytes: delta, ObservedBytes: report.Bytes}
		return nil
	})
	if err == nil {
		serverdiagnostics.ControlUsageReceipt()
	}
	return result, err
}

func validateUsageReport(r UsageReport) error {
	for name, value := range map[string]string{"operation_id": r.OperationID, "edge_node_id": r.EdgeNodeID, "counter_epoch": r.CounterEpoch, "environment_id": r.EnvironmentID, "route_id": r.RouteID, "direction": r.Direction} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", invalidUsage("usage.go:163"), name)
		}
	}
	if r.RouteRevision < 1 || r.Bytes < 0 || (r.Direction != "ingress" && r.Direction != "egress") {
		return fmt.Errorf("%w: invalid counter or direction", invalidUsage("usage.go:167"))
	}
	if r.IntervalStart.IsZero() || r.IntervalEnd.IsZero() || r.IntervalEnd.Before(r.IntervalStart) {
		return fmt.Errorf("%w: invalid interval", invalidUsage("usage.go:170"))
	}
	return nil
}

func sameUsageReport(existing dbsqlc.ControlUsageReceipt, r UsageReport) bool {
	return existing.AuthorityID == r.AuthorityID && existing.EdgeNodeID == r.EdgeNodeID && existing.CounterEpoch == r.CounterEpoch &&
		existing.EnvironmentID == r.EnvironmentID && existing.RouteID == r.RouteID &&
		existing.RouteRevision == r.RouteRevision && existing.Direction == r.Direction &&
		existing.ObservedBytes == r.Bytes
}

func ReconcileScopedUsage(ctx context.Context, store *db.DB, report UsageReport, now time.Time, debiter ScopedBandwidthDebiter) (UsageReceipt, error) {
	if debiter == nil {
		return UsageReceipt{}, invalidUsage("usage.go:184")
	}
	return reconcileUsage(ctx, store, report, now, nil, debiter)
}

// invalidUsage is ErrInvalidUsageReport naming the check that refused the report, so a
// refusal is traceable from the log (every report from an edge was refused unseen).
func invalidUsage(site string) error {
	return fmt.Errorf("%w (%s)", ErrInvalidUsageReport, site)
}
