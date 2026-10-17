package billing

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

var ErrWorkspaceForbidden = errors.New("workspace billing permission denied")

// ReserveUserMachineSeat implements the enrollment authorization boundary.
// An unset personal machine quota permits enrollment for any active account.
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
	policy, err := operatorPolicyTx(ctx, tx)
	if err != nil {
		return err
	}
	if policy.PersonalMachineLimit != nil {
		var count int32
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM user_machines WHERE user_id=$1 AND owner_team_id IS NULL AND seat_state='occupied' AND deleted_at IS NULL`, userID).Scan(&count); err != nil {
			return err
		}
		if count >= *policy.PersonalMachineLimit {
			return ErrWorkspaceQuota
		}
	}
	return nil
}

// Workspace is the billing owner, independent of the member initiating a request.
// Membership and resource grants remain the authority for accessing team resources.
type Workspace struct {
	Product           *WorkspaceProduct `json:"product,omitempty"`
	UsageBytes        int64             `json:"usage_bytes"`
	AllowanceBytes    *int64            `json:"allowance_bytes"`
	ID                string            `json:"id"`
	UserID            string            `json:"user_id,omitempty"`
	TeamID            string            `json:"team_id,omitempty"`
	State             string            `json:"state"`
	ProductCode       string            `json:"product_code,omitempty"`
	Quantity          int               `json:"paid_seats"`
	Members           int               `json:"members"`
	CanManage         bool              `json:"can_manage"`
	RenewalGraceUntil *time.Time        `json:"renewal_grace_until,omitempty"`
	PaidBenefitsUntil *time.Time        `json:"paid_benefits_until,omitempty"`
	PeriodStart       *time.Time        `json:"period_start,omitempty"`
	PeriodEnd         *time.Time        `json:"period_end,omitempty"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	CustomerID        string            `json:"-"`
	SubscriptionID    string            `json:"-"`
	ItemID            string            `json:"-"`
	Revision          int64             `json:"-"`
}

// WorkspaceTx revalidates current role authority even when replaying a provider
// operation. An opaque workspace ID or a paid seat is never administrative proof.
func WorkspaceTx(ctx context.Context, tx *db.Tx, account, team string, manage bool) (Workspace, error) {
	var out Workspace
	if err := teams.Lock(ctx, tx); err != nil {
		return out, err
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
		var owner string
		var member, admin bool
		err := tx.QueryRow(ctx, `SELECT t.owner_account,EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=t.team_id AND m.account_id=$2 AND m.active),EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=t.team_id AND m.account_id=$2 AND m.active AND m.role='admin'),(SELECT count(*) FROM team_members m WHERE m.team_id=t.team_id AND m.active) FROM teams t WHERE t.team_id=$1 AND t.deleted_at IS NULL`, team, account).Scan(&owner, &member, &admin, &out.Members)
		if err == pgx.ErrNoRows {
			return out, ErrWorkspaceForbidden
		}
		if err != nil {
			return out, err
		}
		if !member {
			return out, ErrWorkspaceForbidden
		}
		out.CanManage = owner == account || admin
		if manage && !out.CanManage {
			return out, ErrWorkspaceForbidden
		}
		out.ID, out.UserID, out.TeamID = "team:"+team, "", team
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_workspaces(id,user_id,team_id) VALUES($1,NULLIF($2,''),NULLIF($3,'')) ON CONFLICT DO NOTHING`, out.ID, out.UserID, out.TeamID); err != nil {
		return out, err
	}
	var customer, subscription, item, product sql.NullString
	var start, end, grace, paidUntil sql.NullTime
	err := tx.QueryRow(ctx, `SELECT id,provider_customer_id,provider_subscription_id,provider_item_id,product_code,state,quantity,period_start,period_end,cancel_at_period_end,revision,renewal_grace_until,billing_workspace_paid_until(billing_workspaces) FROM billing_workspaces WHERE (team_id=$1 AND $1<>'' AND user_id IS NULL) OR (user_id=$2 AND $1='' AND team_id IS NULL) FOR UPDATE`, out.TeamID, out.UserID).Scan(&out.ID, &customer, &subscription, &item, &product, &out.State, &out.Quantity, &start, &end, &out.CancelAtPeriodEnd, &out.Revision, &grace, &paidUntil)
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
	return out, err
}
