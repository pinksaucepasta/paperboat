package billing

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var ErrBillingPolicyUnconfigured = errors.New("workspace accounting period is ambiguous")
var ErrWorkspaceQuota = errors.New("workspace quota is exhausted")

func (s *WorkspaceService) PersonalMachineQuota(ctx context.Context) (*int32, error) {
	var out *int32
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		policy, err := operatorPolicyTx(ctx, tx)
		if err != nil {
			return err
		}
		out = policy.PersonalMachineLimit
		return nil
	})
	return out, err
}

func (s *WorkspaceService) usageOverviewTx(ctx context.Context, tx *db.Tx, w *Workspace) error {
	if _, err := s.CheckWorkspaceBandwidthTx(ctx, tx, w.ID, time.Now().UTC()); err != nil {
		return err
	}
	w.AllowanceBytes = nil
	var allowance sql.NullInt64
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(u.observed_bytes),0),max(p.included_bytes)
FROM billing_workspace_usage_periods p JOIN billing_workspace_usage_periods u
 ON u.workspace_id=p.workspace_id AND u.paid=p.paid
 AND coalesce(u.budget_start,u.period_start)=coalesce(p.budget_start,p.period_start)
WHERE p.workspace_id=$1 AND p.period_start<=now() AND p.period_end>now()`, w.ID).Scan(&w.UsageBytes, &allowance)
	if allowance.Valid {
		w.AllowanceBytes = &allowance.Int64
	}
	return err
}

// InitializeAllowancePolicy seeds first-boot configuration without replacing
// later operator changes. Nil means unlimited; zero is an explicit zero limit.
func (s *WorkspaceService) InitializeAllowancePolicy(ctx context.Context, personal, team *int64, machines *int32) error {
	if s.store == nil || (personal != nil && *personal < 0) || (team != nil && *team < 0) || (machines != nil && *machines < 0) {
		return ErrBillingPolicyUnconfigured
	}
	_, err := s.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_operator_policy(singleton,personal_allowance_bytes,team_allowance_bytes,personal_machine_limit) VALUES(true,$1,$2,$3) ON CONFLICT(singleton) DO NOTHING`, personal, team, machines)
	return err
}

// DebitWorkspaceBandwidthTx records every observed byte in the authenticated
// workspace, separately from the amount admitted by a configured allowance.
// The owning usage transaction deduplicates counters and commits its receipt
// atomically with this debit. No provider invoice quantity is changed here.
func (s *WorkspaceService) DebitWorkspaceBandwidthTx(ctx context.Context, tx *db.Tx, workspaceID string, bytes int64, at time.Time) (int64, bool, error) {
	if bytes <= 0 {
		return 0, false, ErrBillingPolicyUnconfigured
	}
	return s.workspaceBandwidthTx(ctx, tx, workspaceID, bytes, at)
}

// CheckWorkspaceBandwidthTx creates the authoritative period before admission.
// Zero allowance denies the first request; nil remains unlimited.
func (s *WorkspaceService) CheckWorkspaceBandwidthTx(ctx context.Context, tx *db.Tx, workspaceID string, at time.Time) (bool, error) {
	_, exhausted, err := s.workspaceBandwidthTx(ctx, tx, workspaceID, 0, at)
	return exhausted, err
}

func (s *WorkspaceService) workspaceBandwidthTx(ctx context.Context, tx *db.Tx, workspaceID string, bytes int64, at time.Time) (int64, bool, error) {
	if tx == nil || workspaceID == "" || bytes < 0 {
		return 0, false, ErrBillingPolicyUnconfigured
	}
	policy, err := workspaceUsagePolicyTx(ctx, tx, workspaceID, at)
	if err != nil {
		return 0, false, err
	}
	window, err := usageWindowTx(ctx, tx, workspaceID, policy, at)
	if err != nil {
		return 0, false, err
	}
	var consumed int64
	if err = tx.QueryRow(ctx, `SELECT coalesce(sum(consumed_bytes),0) FROM billing_workspace_usage_periods
WHERE workspace_id=$1 AND paid=$2 AND coalesce(budget_start,period_start)=$3`, workspaceID, window.paid, window.budget).Scan(&consumed); err != nil {
		return 0, false, err
	}
	included := window.included

	granted := bytes
	if included.Valid {
		granted = min(bytes, max(int64(0), included.Int64-consumed))
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_workspace_usage_periods SET consumed_bytes=consumed_bytes+$4,observed_bytes=observed_bytes+$5 WHERE workspace_id=$1 AND period_start=$2 AND period_end=$3`, workspaceID, window.start, window.end, granted, bytes); err != nil {
		return 0, false, err
	}
	return granted, included.Valid && consumed+granted >= included.Int64, nil
}

func usagePeriodStartTx(ctx context.Context, tx *db.Tx, workspaceID string, start, at time.Time) (time.Time, error) {
	var previous sql.NullTime
	err := tx.QueryRow(ctx, `SELECT max(period_end) FROM billing_workspace_usage_periods
WHERE workspace_id=$1 AND period_end>$2 AND period_end<=$3`, workspaceID, start, at).Scan(&previous)
	if previous.Valid {
		start = previous.Time
	}
	return start, err
}

// Annual payment does not change monthly bandwidth. Preserve the original day
// and clock, clamping short months without letting that clamp move later anchors.
func subscriptionUsageMonth(start, at time.Time) (time.Time, time.Time) {
	start, at = start.UTC(), at.UTC()
	months := (at.Year()-start.Year())*12 + int(at.Month()-start.Month())
	anchor := func(offset int) time.Time {
		month := time.Date(start.Year(), start.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.UTC)
		lastDay := time.Date(month.Year(), month.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
		day := min(start.Day(), lastDay)
		return time.Date(month.Year(), month.Month(), day,
			start.Hour(), start.Minute(), start.Second(), start.Nanosecond(), time.UTC)
	}
	from := anchor(months)
	if from.After(at) {
		months--
		from = anchor(months)
	}
	to := anchor(months + 1)
	return from, to
}

func operatorPolicyTx(ctx context.Context, tx *db.Tx) (OperatorPolicy, error) {
	var out OperatorPolicy
	err := scanPolicy(tx.QueryRow(ctx, `SELECT personal_allowance_bytes,team_allowance_bytes,personal_machine_limit,revision FROM billing_operator_policy WHERE singleton`), &out)
	if err == pgx.ErrNoRows {
		return out, ErrBillingPolicyUnconfigured
	}
	return out, err
}
