package billing

import (
	"context"
	"database/sql"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

type paidTeamProvider interface {
	PaidTeamCheckout(context.Context, string) (StripeCheckoutState, string, error)
}

// handleTeamCreationWebhook returns handled separately from processed so other
// billing webhook owners can dispatch without acknowledging an early event.
func (s *WorkspaceService) handleTeamCreationWebhook(ctx context.Context, event StripeWebhook) (bool, bool, error) {
	var key string
	var sub StripeSubscription
	var err error
	if event.Data.Object.Object == "checkout.session" {
		err = s.store.SQL().QueryRowContext(ctx, `SELECT operation_id FROM paperboat.billing_team_orders WHERE provider_session_id=$1`, event.Data.Object.ID).Scan(&key)
		if err == pgx.ErrNoRows || err == sql.ErrNoRows {
			// A callback may beat persistence of the Checkout response. Retrieve its
			// operation metadata rather than dropping the only paid callback.
			p, ok := s.stripe.(paidTeamProvider)
			if !ok {
				return false, false, nil
			}
			state, _, e := p.PaidTeamCheckout(ctx, event.Data.Object.ID)
			if e != nil {
				return true, false, e
			}
			if !strings.HasPrefix(state.CheckoutOperation, "team-create:") {
				return false, false, nil
			}
			key = strings.TrimPrefix(state.CheckoutOperation, "team-create:")
		} else if err != nil {
			return true, false, err
		}
	} else if event.SubscriptionID() != "" {
		sub, err = s.stripe.GetSubscription(ctx, event.SubscriptionID())
		if err != nil {
			return true, false, err
		}
		if !strings.HasPrefix(sub.CheckoutOperation, "team-create:") {
			return false, false, nil
		}
		key = strings.TrimPrefix(sub.CheckoutOperation, "team-create:")
	} else {
		return false, false, nil
	}
	var cipher []byte
	var session sql.NullString
	var state string
	err = s.store.SQL().QueryRowContext(ctx, `SELECT request_ciphertext,provider_session_id,state FROM paperboat.billing_team_orders WHERE operation_id=$1`, key).Scan(&cipher, &session, &state)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return true, false, err
	}
	if state == "failed" {
		return true, false, nil
	}
	if state == "paid" {
		return false, false, nil
	}
	var req teamCreationRequest
	if err = s.decryptResult(cipher, &req); err != nil {
		return true, false, err
	}
	id := session.String
	if event.Data.Object.Object == "checkout.session" {
		id = event.Data.Object.ID
	}
	if id == "" {
		return true, false, ErrProviderOutcomeUnknown
	}
	p, ok := s.stripe.(paidTeamProvider)
	if !ok {
		return true, false, ErrStripeUnavailable
	}
	checkout, paid, err := p.PaidTeamCheckout(ctx, id)
	if err != nil {
		return true, false, err
	}
	if checkout.ID != id || checkout.CustomerID != req.CustomerID || checkout.CheckoutOperation != "team-create:"+key || checkout.Mode != "subscription" || checkout.PriceID != req.PriceID || checkout.Quantity != req.Input.Seats {
		return true, false, ErrProviderOutcomeUnknown
	}
	if checkout.Status == "expired" {
		err = s.expireTeamOrder(ctx, key)
		return true, err == nil, err
	}
	// A trial Checkout that collected a card reports "paid" (Stripe sandbox, 2026-10-23);
	// one without a first charge may report "no_payment_required". Both start a trial.
	paymentOK, wantStatus := paid == "paid", "active"
	if req.TrialDays > 0 {
		paymentOK, wantStatus = paid == "paid" || paid == "no_payment_required", "trialing"
	}
	if checkout.Status != "complete" || !paymentOK || checkout.SubscriptionID == "" {
		return true, false, ErrCheckoutPending
	}
	if event.Data.Object.Object != "checkout.session" && event.SubscriptionID() != checkout.SubscriptionID {
		return true, false, ErrProviderOutcomeUnknown
	}
	sub, err = s.stripe.GetSubscription(ctx, checkout.SubscriptionID)
	if err != nil {
		return true, false, err
	}
	if sub.ID != checkout.SubscriptionID || sub.CustomerID != req.CustomerID || sub.PriceID != req.PriceID || sub.Quantity != req.Input.Seats || sub.CheckoutOperation != "team-create:"+key || sub.Status != wantStatus || sub.ItemID == "" || !sub.PeriodEnd.After(sub.PeriodStart) {
		return true, false, ErrProviderOutcomeUnknown
	}
	processed, err := s.materializePaidTeam(ctx, event, key, id, req, sub)
	return true, processed, err
}

func (s *WorkspaceService) materializePaidTeam(ctx context.Context, event StripeWebhook, key, id string, req teamCreationRequest, sub StripeSubscription) (bool, error) {
	var state string
	processed := false
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state FROM billing_team_orders WHERE operation_id=$1 FOR UPDATE`, key).Scan(&state); err != nil {
			return err
		}
		if state == "failed" {
			return ErrWorkspaceOperationFailed
		}
		if state == "paid" {
			return nil
		}
		var actor string
		if err := tx.QueryRow(ctx, `SELECT actor_user_id FROM billing_team_orders WHERE operation_id=$1`, key).Scan(&actor); err != nil {
			return err
		}
		if err := activeTeamCreator(ctx, tx, actor); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, req.Input.TeamID, actor); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, req.Input.TeamID, actor); err != nil {
			return err
		}
		if err := teams.InitTeamPolicyTx(ctx, tx, req.Input.TeamID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_workspaces(id,team_id,provider_customer_id,provider_subscription_id,provider_item_id,product_code,state,quantity,period_start,period_end,bandwidth_anchor,cancel_at_period_end) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$9,$11)`, "team:"+req.Input.TeamID, req.Input.TeamID, sub.CustomerID, sub.ID, sub.ItemID, req.ProductCode, sub.Status, sub.Quantity, sub.PeriodStart, sub.PeriodEnd, sub.CancelAtPeriodEnd); err != nil {
			return err
		}
		if err := teams.AuditTx(ctx, tx, actor, req.Input.TeamID, "create", key, nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_team_orders SET state='paid',provider_session_id=$2,updated_at=now() WHERE operation_id=$1`, key, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO stripe_events(id,event_type,subscription_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, event.ID, event.Type, sub.ID)
		processed = err == nil
		return err
	})
	return processed, err
}
