package operator

import (
	"context"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

// Entitlements is what an operator sees for one workspace: its bandwidth allowance with what operators granted, every counted
// limit with the plan's value and any override, team seats, and the grants themselves (running ones first).
type Entitlements struct {
	WorkspaceID string                `json:"workspace_id"`
	Bandwidth   BandwidthEntitlement  `json:"bandwidth"`
	Limits      []planlimits.LimitRow `json:"limits"`
	Seats       *SeatsEntitlement     `json:"seats,omitempty"`
	Grants      []planlimits.Grant    `json:"grants"`
}

// BandwidthEntitlement: PlanBytes is the allowance of the current usage period before grants (null: unlimited, or no period
// has started yet); GrantedBytes what running grants add; UsageBytes what the workspace used in the period.
type BandwidthEntitlement struct {
	PlanBytes    *int64 `json:"plan_bytes"`
	GrantedBytes int64  `json:"granted_bytes"`
	UsageBytes   int64  `json:"usage_bytes"`
}

type SeatsEntitlement struct {
	Bought   int  `json:"bought"`
	Used     int  `json:"used"`
	Override *int `json:"override,omitempty"`
}

func readEntitlements(ctx context.Context, tx *db.Tx, workspaceID string) (Entitlements, error) {
	out := Entitlements{WorkspaceID: workspaceID, Limits: []planlimits.LimitRow{}}
	team := strings.HasPrefix(workspaceID, "team:")
	var exists bool
	query, arg := `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, strings.TrimPrefix(workspaceID, "personal:")
	if team {
		query, arg = `SELECT EXISTS(SELECT 1 FROM teams WHERE team_id=$1)`, strings.TrimPrefix(workspaceID, "team:")
	}
	if !team && !strings.HasPrefix(workspaceID, "personal:") {
		return out, ErrInvalid
	}
	if err := tx.QueryRow(ctx, query, arg).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, ErrNotFound
	}
	var err error
	if out.Limits, err = planlimits.LimitsTx(ctx, tx, workspaceID); err != nil {
		return out, err
	}
	if out.Grants, err = planlimits.GrantsTx(ctx, tx, workspaceID); err != nil {
		return out, err
	}
	now := time.Now().UTC()
	if err = tx.QueryRow(ctx, `SELECT workspace_granted_bytes($1),
 coalesce((SELECT sum(u.observed_bytes) FROM billing_workspace_usage_periods p JOIN billing_workspace_usage_periods u ON u.workspace_id=p.workspace_id AND u.paid=p.paid
   AND coalesce(u.budget_start,u.period_start)=coalesce(p.budget_start,p.period_start) WHERE p.workspace_id=$1 AND p.period_start<=$2 AND p.period_end>$2),0),
 (SELECT max(p.included_bytes) FROM billing_workspace_usage_periods p WHERE p.workspace_id=$1 AND p.period_start<=$2 AND p.period_end>$2)`, workspaceID, now).
		Scan(&out.Bandwidth.GrantedBytes, &out.Bandwidth.UsageBytes, &out.Bandwidth.PlanBytes); err != nil {
		return out, err
	}
	if out.Bandwidth.PlanBytes == nil {
		out.Bandwidth.GrantedBytes = 0 // an unlimited allowance stays unlimited: the customer's overview shows no grant either
	}
	if team {
		var seats SeatsEntitlement
		if err = tx.QueryRow(ctx, `SELECT coalesce((SELECT quantity FROM billing_workspaces WHERE team_id=$1 AND team_paid_entitled($1)),0),
 (SELECT count(*) FROM team_members WHERE team_id=$1 AND active)+(SELECT count(*) FROM team_invitations WHERE team_id=$1 AND accepted_at IS NULL AND cancelled_at IS NULL AND expires_at>now())`, arg).Scan(&seats.Bought, &seats.Used); err != nil {
			return out, err
		}
		if value, ok, err := planlimits.SeatsOverrideTx(ctx, tx, arg); err != nil {
			return out, err
		} else if ok {
			seats.Override = &value
		}
		out.Seats = &seats
	}
	return out, nil
}
