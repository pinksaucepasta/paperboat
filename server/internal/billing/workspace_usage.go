package billing

import (
	"context"
	"database/sql"
	"encoding/json"
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
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(observed_bytes),0),max(included_bytes) FROM billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=now() AND period_end>now()`, w.ID).Scan(&w.UsageBytes, &allowance)
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
	var team, catalog, interval sql.NullString
	var start, end sql.NullTime
	if err := tx.QueryRow(ctx, `SELECT w.team_id,w.period_start,w.period_end,p.catalog_ref,p.verified_interval
FROM billing_workspaces w LEFT JOIN billing_products p
 ON p.code=w.product_code AND p.provider='stripe'
 AND w.period_start<=$2 AND w.period_end>$2
WHERE w.id=$1 FOR UPDATE OF w`, workspaceID, at).Scan(&team, &start, &end, &catalog, &interval); err != nil {
		return 0, false, err
	}
	policy, err := operatorPolicyTx(ctx, tx)
	if err != nil {
		return 0, false, err
	}
	allowance := policy.PersonalAllowanceBytes
	if team.Valid {
		allowance = policy.TeamAllowanceBytes
	}
	if catalog.Valid {
		var policy struct {
			AllowanceBytes *int64 `json:"allowance_bytes"`
		}
		if err := json.Unmarshal([]byte(catalog.String), &policy); err != nil {
			return 0, false, err
		}
		if policy.AllowanceBytes != nil {
			allowance = policy.AllowanceBytes
		}
	}
	if allowance != nil && *allowance < 0 {
		return 0, false, ErrBillingPolicyUnconfigured
	}
	// Personal Free has no provider period. Use UTC calendar months for its usage
	// display while retaining prior periods for delayed reports after plan changes.
	periodStart := time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	if start.Valid && end.Valid && !at.Before(start.Time) && at.Before(end.Time) {
		periodStart, periodEnd = start.Time, end.Time
		if interval.String == "year" {
			periodStart, periodEnd = subscriptionUsageMonth(periodStart, periodEnd, at)
		}
	}
	var recordedStart, recordedEnd time.Time
	var included sql.NullInt64
	var consumed int64
	rows, err := tx.Query(ctx, `SELECT period_start,period_end,included_bytes,consumed_bytes FROM billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=$2 AND period_end>$2 FOR UPDATE`, workspaceID, at)
	if err != nil {
		return 0, false, err
	}
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&recordedStart, &recordedEnd, &included, &consumed); err != nil {
			rows.Close()
			return 0, false, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, false, err
	}
	if count > 1 {
		return 0, false, ErrBillingPolicyUnconfigured
	}
	// A subscription can start inside an already opened Free month. Close its
	// predecessor at that boundary so delayed reports retain their old allowance
	// and new traffic receives the provider's complete paid period.
	if count == 1 && catalog.Valid && recordedStart.Before(periodStart) && recordedEnd.After(periodStart) {
		if _, err = tx.Exec(ctx, `UPDATE billing_workspace_usage_periods SET period_end=$4
WHERE workspace_id=$1 AND period_start=$2 AND period_end=$3`,
			workspaceID, recordedStart, recordedEnd, periodStart); err != nil {
			return 0, false, err
		}
		count, consumed, included = 0, 0, sql.NullInt64{}
	}
	if count == 0 {
		recordedStart, err = usagePeriodStartTx(ctx, tx, workspaceID, periodStart, at)
		if err != nil {
			return 0, false, err
		}
		recordedEnd = periodEnd
		if allowance != nil {
			included = sql.NullInt64{Int64: *allowance, Valid: true}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes) VALUES($1,$2,$3,$4)`, workspaceID, recordedStart, recordedEnd, included); err != nil {
			return 0, false, err
		}
	}
	granted := bytes
	if included.Valid {
		granted = min(bytes, max(int64(0), included.Int64-consumed))
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_workspace_usage_periods SET consumed_bytes=consumed_bytes+$4,observed_bytes=observed_bytes+$5 WHERE workspace_id=$1 AND period_start=$2 AND period_end=$3`, workspaceID, recordedStart, recordedEnd, granted, bytes); err != nil {
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
func subscriptionUsageMonth(start, end, at time.Time) (time.Time, time.Time) {
	start, end, at = start.UTC(), end.UTC(), at.UTC()
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
	if to.After(end) {
		to = end
	}
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
