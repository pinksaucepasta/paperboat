package billing

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

var ErrWorkspaceForbidden = errors.New("workspace billing permission denied")

// ReserveUserMachineSeat implements the enrollment authorization boundary: an active account
// within its plan's machine limit (planlimits) may enroll one more personal machine.
func (s *WorkspaceService) ReserveUserMachineSeat(ctx context.Context, tx *db.Tx, userID string) error {
	if tx == nil {
		return ErrWorkspaceForbidden
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status); err != nil {
		if err == pgx.ErrNoRows {
			return ErrWorkspaceForbidden
		}
		return err
	}
	if status != "active" {
		return ErrWorkspaceForbidden
	}
	return planlimits.CheckTx(ctx, tx, planlimits.Scope{Account: userID}, planlimits.Machines)
}

// Workspace is the billing owner, independent of the member initiating a request.
// Membership and resource grants remain the authority for accessing team resources.
type Workspace struct {
	TopupBytesRemaining  int64             `json:"topup_bytes_remaining"`
	TopupBytesPurchased  int64             `json:"topup_bytes_purchased"`
	Topups               []WorkspaceTopup  `json:"topups"`
	AllowancePeriodStart time.Time         `json:"allowance_period_start"`
	AllowancePeriodEnd   time.Time         `json:"allowance_period_end"`
	Product              *WorkspaceProduct `json:"product,omitempty"`
	UsageBytes           int64             `json:"usage_bytes"`
	AllowanceBytes       *int64            `json:"allowance_bytes"`
	// AllowanceGrantedBytes is the part of AllowanceBytes an operator granted (it is included in AllowanceBytes).
	AllowanceGrantedBytes int64 `json:"allowance_granted_bytes,omitempty"`
	// Throttled: the fair-use amount is used up, so edge traffic runs at the throttled rate until AllowancePeriodEnd.
	// ThrottleLifted: an operator lifted it (the workspace runs at full speed meanwhile).
	Throttled      bool `json:"throttled"`
	ThrottleLifted bool `json:"throttle_lifted"`
	// ThrottleMbit is the speed a throttled workspace's edge traffic runs at (the edges' configured rate).
	ThrottleMbit float64 `json:"throttle_mbit"`
	// SeatsGranted is an operator's override of the team's seats, replacing what it bought.
	SeatsGranted      *int       `json:"seats_granted,omitempty"`
	ID                string     `json:"id"`
	UserID            string     `json:"user_id,omitempty"`
	TeamID            string     `json:"team_id,omitempty"`
	State             string     `json:"state"`
	ProductCode       string     `json:"product_code,omitempty"`
	SeatsUsed         int        `json:"seats_used"`
	ScheduledQuantity *int       `json:"scheduled_seats,omitempty"`
	ScheduledAt       *time.Time `json:"scheduled_seats_at,omitempty"`
	Quantity          int        `json:"seats_bought"`
	Members           int        `json:"members"`
	CanManage         bool       `json:"can_manage"`
	RenewalGraceUntil *time.Time `json:"renewal_grace_until,omitempty"`
	PaidBenefitsUntil *time.Time `json:"paid_benefits_until,omitempty"`
	PeriodStart       *time.Time `json:"period_start,omitempty"`
	PeriodEnd         *time.Time `json:"period_end,omitempty"`
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	// Granted is a running support grant: an active plan with no Stripe subscription behind it, ending at GrantedUntil.
	Granted      bool       `json:"granted"`
	GrantedUntil *time.Time `json:"granted_until,omitempty"`
	// Limits are the plan's count limits with current use (machines, publications = previews and
	// tunnels, browser_terminals); a Teams limit is null (advertised unlimited).
	Limits map[planlimits.Kind]planlimits.Usage `json:"limits"`
	// TeamTrialDays, on the personal overview only, is the trial a new team would get.
	TeamTrialDays  *int   `json:"team_trial_days,omitempty"`
	CustomerID     string `json:"-"`
	SubscriptionID string `json:"-"`
	ItemID         string `json:"-"`
	Revision       int64  `json:"-"`
}

// WorkspaceTx revalidates current role authority even when replaying a provider
// operation. An opaque workspace ID or a paid seat is never administrative proof.
// manage is the write path: it serializes with team writes and locks the workspace row.
// A read uses the transaction's snapshot and blocks no team write.
func WorkspaceTx(ctx context.Context, tx *db.Tx, account, team string, manage bool) (Workspace, error) {
	var out Workspace
	if manage {
		if err := teams.LockFor(ctx, tx, team); err != nil {
			return out, err
		}
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`, account).Scan(&active); err != nil {
		return out, err
	}
	if !active {
		return out, ErrWorkspaceForbidden
	}
	out.ID, out.UserID, out.CanManage = "personal:"+account, account, true
	if team != "" {
		var member bool
		// The owner and billing admins manage billing (teams.PermissionBillingManage).
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=t.team_id AND m.account_id=$2 AND m.active),coalesce((SELECT team_role_allows(CASE WHEN t.owner_account=$2 THEN 'owner' ELSE m.role END,'billing.manage') FROM team_members m WHERE m.team_id=t.team_id AND m.account_id=$2 AND m.active),false),(SELECT count(*) FROM team_members m WHERE m.team_id=t.team_id AND m.active) FROM teams t WHERE t.team_id=$1 AND t.deleted_at IS NULL`, team, account).Scan(&member, &out.CanManage, &out.Members)
		if err == pgx.ErrNoRows {
			return out, ErrWorkspaceForbidden
		}
		if err != nil {
			return out, err
		}
		if !member {
			return out, ErrWorkspaceForbidden
		}
		if manage && !out.CanManage {
			return out, ErrWorkspaceForbidden
		}
		out.ID, out.UserID, out.TeamID = "team:"+team, "", team
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_workspaces(id,user_id,team_id) VALUES($1,NULLIF($2,''),NULLIF($3,'')) ON CONFLICT DO NOTHING`, out.ID, out.UserID, out.TeamID); err != nil {
		return out, err
	}
	var customer, subscription, item, product sql.NullString
	var start, end, grace, paidUntil, scheduledAt sql.NullTime
	var scheduled sql.NullInt32
	var grantedUntil sql.NullTime
	err := tx.QueryRow(ctx, `SELECT id,provider_customer_id,provider_subscription_id,provider_item_id,product_code,state,quantity,period_start,period_end,cancel_at_period_end,revision,renewal_grace_until,billing_workspace_paid_until(billing_workspaces),scheduled_quantity,scheduled_quantity_at,CASE WHEN grant_until>now() AND state='active' AND provider_subscription_id IS NULL THEN grant_until END FROM billing_workspaces WHERE (team_id=$1 AND $1<>'' AND user_id IS NULL) OR (user_id=$2 AND $1='' AND team_id IS NULL)`+lockClause(manage), out.TeamID, out.UserID).Scan(&out.ID, &customer, &subscription, &item, &product, &out.State, &out.Quantity, &start, &end, &out.CancelAtPeriodEnd, &out.Revision, &grace, &paidUntil, &scheduled, &scheduledAt, &grantedUntil)
	out.CustomerID, out.SubscriptionID, out.ItemID, out.ProductCode = customer.String, subscription.String, item.String, product.String
	if start.Valid {
		out.PeriodStart = &start.Time
	}
	if end.Valid {
		out.PeriodEnd = &end.Time
	}
	if grace.Valid {
		out.RenewalGraceUntil = &grace.Time
	}
	if paidUntil.Valid {
		out.PaidBenefitsUntil = &paidUntil.Time
	}
	if grantedUntil.Valid {
		out.Granted, out.GrantedUntil = true, &grantedUntil.Time
	}
	if err != nil {
		return out, err
	}
	if out.TeamID != "" {
		if scheduled.Valid {
			n := int(scheduled.Int32)
			out.ScheduledQuantity = &n
		}
		if scheduledAt.Valid {
			out.ScheduledAt = &scheduledAt.Time
		}
		out.SeatsUsed, err = teams.SeatUsageTx(ctx, tx, out.TeamID)
	}
	return out, err
}

func lockClause(manage bool) string {
	if manage {
		return " FOR UPDATE"
	}
	return ""
}
