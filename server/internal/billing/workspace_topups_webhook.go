package billing

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// handleTopupWebhook runs only after the HTTP boundary authenticates Stripe's
// signature. Every fulfillment fact is fetched again from the provider.
func (s *WorkspaceService) handleTopupWebhook(ctx context.Context, event StripeWebhook) (handled, processed bool, err error) {
	if event.Data.Object.Object != "checkout.session" || (event.Type != "checkout.session.completed" && event.Type != "checkout.session.async_payment_succeeded") {
		return false, false, nil
	}
	if event.Data.Object.Mode != "" && event.Data.Object.Mode != "payment" {
		return false, false, nil
	}
	provider, ok := s.stripe.(StripeTopupClient)
	if !ok {
		return false, false, nil
	}
	state, err := provider.GetTopupCheckout(ctx, event.Data.Object.ID)
	if err != nil {
		return true, false, err
	}
	if state.Mode != "payment" {
		return false, false, nil
	}
	if event.ID == "" || state.Operation == "" {
		return true, false, nil
	}
	paidAt := time.Unix(event.Created, 0).UTC()
	expires := time.Date(paidAt.Year(), paidAt.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC()
	if event.Created <= 0 || paidAt.Before(state.CreatedAt) || paidAt.After(now.Add(5*time.Minute)) || !now.Before(expires) {
		return true, false, nil
	}
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {

		// Serialize the operation receipt, workspace entitlement and credit together.
		var w Workspace
		var live bool
		e := tx.QueryRow(ctx, `SELECT w.id,coalesce(w.product_code,''),coalesce(w.provider_customer_id,''),w.period_start,billing_workspace_paid_until(w),
 CASE WHEN w.team_id IS NULL THEN EXISTS(SELECT 1 FROM users u WHERE u.id=w.user_id AND u.status='active') ELSE EXISTS(SELECT 1 FROM teams t WHERE t.team_id=w.team_id AND t.deleted_at IS NULL) END
 FROM billing_workspaces w WHERE w.id=$1 FOR UPDATE`, state.WorkspaceID).Scan(&w.ID, &w.ProductCode, &w.CustomerID, &w.PeriodStart, &w.PaidBenefitsUntil, &live)
		if e == pgx.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		var duplicate bool
		if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_workspace_topup_events WHERE event_id=$1)`, event.ID).Scan(&duplicate); e != nil || duplicate {
			return e
		}
		op, found, e := loadWorkspaceOperation(ctx, tx, state.Operation)
		if e != nil {
			return e
		}
		if !found || op.WorkspaceID != w.ID || op.Kind != "topup" {
			return nil
		}
		if op.State == "pending" || op.State == "uncertain" {
			return ErrProviderOutcomeUnknown
		}
		if op.State != "succeeded" {
			return nil
		}
		var request topupRequest
		var receipt CheckoutSession
		if e = s.decryptResult(op.RequestCiphertext, &request); e != nil {
			return e
		}
		if e = s.decryptResult(op.Ciphertext, &receipt); e != nil {
			return e
		}
		if !live || !paidTopupWorkspace(w, paidAt) || !paidTopupWorkspace(w, now) || !topupIdentity(state, request) || receipt.ProviderSessionID != state.ID || receipt.ProviderCustomerID != state.CustomerID || request.BytesAmount <= 0 {
			return nil
		}
		if !op.ProviderAttemptedAt.Valid || paidAt.Before(op.ProviderAttemptedAt.Time.Truncate(time.Second)) {
			return nil
		}
		expectsPaid := event.Type == "checkout.session.async_payment_succeeded" || event.Data.Object.PaymentStatus == "paid"
		if expectsPaid && (state.Status != "complete" || state.PaymentStatus != "paid") {
			return ErrProviderOutcomeUnknown
		}
		if expectsPaid {
			_, e = tx.Exec(ctx, `INSERT INTO billing_workspace_topups(provider_session_id,operation_key,workspace_id,purchased_bytes,remaining_bytes,purchased_at,expires_at) VALUES($1,$2,$3,$4,$4,$5,$6) ON CONFLICT DO NOTHING`, state.ID, state.Operation, w.ID, request.BytesAmount, paidAt, expires)
			if e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, `INSERT INTO billing_workspace_topup_events(event_id,provider_session_id,event_type) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, event.ID, state.ID, event.Type)
		processed = e == nil
		return e
	})
	return true, processed, err
}
