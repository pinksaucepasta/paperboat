package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type usagePolicy struct {
	start, end, paidFrom time.Time
	providerEnd          sql.NullTime
	paid                 bool
	included             *int64
}

type usageWindow struct {
	start, end, budget time.Time
	paid               bool
	included           sql.NullInt64
}

func workspaceUsagePolicyTx(ctx context.Context, tx *db.Tx, id string, at time.Time) (usagePolicy, error) {
	var team, catalog sql.NullString
	var start, end, anchor sql.NullTime
	var out usagePolicy
	err := tx.QueryRow(ctx, `SELECT w.team_id,w.period_start,w.period_end,w.bandwidth_anchor,p.catalog_ref
FROM billing_workspaces w LEFT JOIN billing_products p
 ON p.code=w.product_code AND p.provider='stripe' AND w.period_start<=$2 AND w.period_end>$2
WHERE w.id=$1 FOR UPDATE OF w`, id, at).Scan(&team, &start, &end, &anchor, &catalog)
	if err != nil {
		return out, err
	}
	policy, err := operatorPolicyTx(ctx, tx)
	if err != nil {
		return out, err
	}
	out.included = policy.PersonalAllowanceBytes
	if team.Valid {
		out.included = policy.TeamAllowanceBytes
	}
	out.start = time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	out.end = out.start.AddDate(0, 1, 0)
	out.providerEnd = end
	if start.Valid && end.Valid && !at.Before(start.Time) && at.Before(end.Time) {
		out.start, out.end = start.Time, end.Time
	}
	if catalog.Valid {
		var product struct {
			AllowanceBytes *int64 `json:"allowance_bytes"`
		}
		if err = json.Unmarshal([]byte(catalog.String), &product); err != nil {
			return out, err
		}
		if product.AllowanceBytes != nil {
			out.included = product.AllowanceBytes
		}
		out.paid = true
		out.paidFrom = start.Time
		if !anchor.Valid {
			anchor = start
			if _, err = tx.Exec(ctx, `UPDATE billing_workspaces SET bandwidth_anchor=$2 WHERE id=$1`, id, anchor.Time); err != nil {
				return out, err
			}
		}
		out.start, out.end = subscriptionUsageMonth(anchor.Time, at)
	}
	if out.included != nil && *out.included < 0 {
		return out, ErrBillingPolicyUnconfigured
	}
	return out, nil
}

func usageWindowTx(ctx context.Context, tx *db.Tx, id string, policy usagePolicy, at time.Time) (usageWindow, error) {
	var out usageWindow
	// A paid budget has a stable monthly end, independent of partial provider
	// periods. Close its historical segment only when entitlement actually ends.
	if policy.providerEnd.Valid && !at.Before(policy.providerEnd.Time) {
		if _, err := tx.Exec(ctx, `UPDATE billing_workspace_usage_periods SET period_end=$2
WHERE workspace_id=$1 AND paid AND period_start<$2 AND period_end>$2`, id, policy.providerEnd.Time); err != nil {
			return out, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT period_start,period_end,coalesce(budget_start,period_start),paid,included_bytes
FROM billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=$2 AND period_end>$2 FOR UPDATE`, id, at)
	if err != nil {
		return out, err
	}
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&out.start, &out.end, &out.budget, &out.paid, &out.included); err != nil {
			rows.Close()
			return out, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if count > 1 {
		return out, ErrBillingPolicyUnconfigured
	}
	if count == 1 && policy.paid && !out.paid && out.start.Before(policy.paidFrom) && out.end.After(policy.paidFrom) {
		if _, err = tx.Exec(ctx, `UPDATE billing_workspace_usage_periods SET period_end=$3 WHERE workspace_id=$1 AND period_start=$2`, id, out.start, policy.paidFrom); err != nil {
			return out, err
		}
		count = 0
	}
	if count == 1 {
		return out, nil
	}
	out = usageWindow{start: policy.start, end: policy.end, budget: policy.start, paid: policy.paid}
	if policy.paid && out.start.Before(policy.paidFrom) {
		out.start = policy.paidFrom
	}
	out.start, err = usagePeriodStartTx(ctx, tx, id, out.start, at)
	if err != nil {
		return out, err
	}
	if policy.included != nil {
		out.included = sql.NullInt64{Int64: *policy.included, Valid: true}
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,paid,budget_start) VALUES($1,$2,$3,$4,$5,$6)`, id, out.start, out.end, out.included, out.paid, out.budget)
	return out, err
}
