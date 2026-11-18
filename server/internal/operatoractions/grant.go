package operatoractions

import (
	"context"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// A plan grant (a support comp) is an active subscription row of the workspace with no Stripe subscription: the
// same row a paid plan sets, so every reader of the effective plan and allowance (planlimits, team_paid_entitled,
// usage periods, the peer-policy triggers) treats it exactly like a paid one, and billing_workspaces stays the one
// owner of the effective plan. It expires on its own at period_end. grant_until marks it running: while it does,
// reconcileSubscription applies a Stripe snapshot only if it is a live paid subscription (which ends the grant), so a
// retried or replayed event of any age for an old subscription cannot overwrite it. Granting over a lapsed Stripe
// record keeps the customer id and clears the subscription and item ids. This is also how the test fixtures give a
// team its paid entitlement (testfixtures.CreateTeam).
const (
	maxGrantDays = 366
)

func isGrantKind(kind string) bool { return kind == "plan_grant" || kind == "plan_revoke" }

// workspaceOwner reads "personal:<account>" or "team:<team>" into the billing_workspaces owner columns.
func workspaceOwner(target string) (user, team string, ok bool) {
	switch {
	case strings.HasPrefix(target, "personal:") && len(target) > len("personal:"):
		return strings.TrimPrefix(target, "personal:"), "", true
	case strings.HasPrefix(target, "team:") && len(target) > len("team:"):
		return "", strings.TrimPrefix(target, "team:"), true
	}
	return "", "", false
}

func grantAction(ctx context.Context, tx *db.Tx, kind, target string, in Input, out *Result) error {
	user, team, ok := workspaceOwner(target)
	if !ok {
		return ErrInvalid
	}
	if kind == "plan_grant" {
		var catalog string
		scope := map[bool]string{false: "personal_subscription", true: "team_subscription"}[team != ""]
		if err := tx.QueryRow(ctx, `SELECT catalog_type FROM billing_products WHERE code=$1 AND provider='stripe' AND active`, in.ProductCode).Scan(&catalog); err != nil {
			if operatorNoRowsOnly(err) {
				return ErrNotFound
			}
			return err
		}
		if catalog != scope || (team == "") != (in.Quantity == 0) {
			return ErrInvalid // a personal plan for a personal workspace (no seats), a team plan for a team (seats)
		}
		var owner bool
		query := `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`
		arg := user
		if team != "" {
			if err := teams.LockFor(ctx, tx, team); err != nil {
				return err
			}
			query, arg = `SELECT EXISTS(SELECT 1 FROM teams WHERE team_id=$1 AND deleted_at IS NULL AND NOT suspended)`, team
		}
		if err := tx.QueryRow(ctx, query, arg).Scan(&owner); err != nil {
			return err
		}
		if !owner {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_workspaces(id,user_id,team_id) VALUES($1,NULLIF($2,''),NULLIF($3,'')) ON CONFLICT DO NOTHING`, target, user, team); err != nil {
			return err
		}
	}
	var subscription, customer *string
	var state string
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT provider_subscription_id,provider_customer_id,state,revision FROM billing_workspaces WHERE id=$1 FOR UPDATE`, target).Scan(&subscription, &customer, &state, &revision); err != nil {
		if operatorNoRowsOnly(err) {
			return ErrNotFound
		}
		return err
	}
	// The Stripe webhook takes this lock too (billing.reconcileSubscription).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, target); err != nil {
		return err
	}
	if revision != in.Revision {
		return ErrConflict
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_workspace_operations WHERE workspace_id=$1 AND state IN ('pending','uncertain'))`, target).Scan(&pending); err != nil {
		return err
	}
	if kind == "plan_revoke" {
		// Only a grant: an active row that has no Stripe subscription (a paid plan is cancelled in Stripe), and not
		// while a checkout is in flight, whose subscription would otherwise be applied over the revoke.
		if subscription != nil || state != "active" || pending {
			return ErrConflict
		}
		revoke := `UPDATE billing_workspaces SET state='canceled',cancel_at_period_end=false,grant_until=NULL,revision=revision+1,updated_at=now() WHERE id=$1`
		if customer == nil {
			// A workspace that never had a Stripe customer has no billing history to keep: back to a plain free one.
			revoke = `UPDATE billing_workspaces SET state='free',product_code=NULL,quantity=0,period_start=NULL,period_end=NULL,cancel_at_period_end=false,grant_until=NULL,revision=revision+1,updated_at=now() WHERE id=$1`
		}
		if _, err := tx.Exec(ctx, revoke, target); err != nil {
			return err
		}
		*out = Result{ResourceID: target, State: "revoked", Version: revision + 1}
		return nil
	}
	// A live Stripe subscription, or a checkout still in flight, owns this workspace. A cancelled or expired one does
	// not (checkout allows the same), so a lapsed customer can be comped at once.
	if pending || (subscription != nil && (state != "canceled" && state != "incomplete_expired")) {
		return ErrConflict
	}
	var end time.Time
	if err := tx.QueryRow(ctx, `UPDATE billing_workspaces SET product_code=$2,state='active',quantity=$3,period_start=now(),period_end=now()+make_interval(days => $4),cancel_at_period_end=true,grant_until=now()+make_interval(days => $4),provider_subscription_id=NULL,provider_item_id=NULL,scheduled_quantity=NULL,scheduled_quantity_at=NULL,pending_seat_capacity=NULL,renewal_grace_until=NULL,renewal_grace_invoice_id=NULL,revision=revision+1,updated_at=now() WHERE id=$1 RETURNING period_end`, target, in.ProductCode, in.Quantity, in.Days).Scan(&end); err != nil {
		return err
	}
	*out = Result{ResourceID: target, State: "granted", Version: revision + 1, ExpiresAt: &end}
	return nil
}

// auditMetadata is what the audit event records: the reason and outcome, and for a plan grant what was granted.
func auditMetadata(kind, reason string, in Input, result Result) map[string]any {
	metadata := map[string]any{"reason": reason, "state": result.State, "version": result.Version}
	if kind == "entitlement_grant" {
		metadata["entitlement"], metadata["amount"], metadata["ends_at"], metadata["grant_id"] = in.Entitlement, in.Amount, in.EndsAt, result.GrantID
	}
	if kind == "entitlement_revoke" {
		metadata["grant_id"] = result.GrantID
	}
	if kind == "plan_grant" {
		metadata["product_code"], metadata["days"], metadata["quantity"], metadata["expires_at"] = in.ProductCode, in.Days, in.Quantity, result.ExpiresAt
	}
	return metadata
}
