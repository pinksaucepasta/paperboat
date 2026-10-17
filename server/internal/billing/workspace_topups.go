package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var ErrTopupRequiresPaid = errors.New("top-ups require a paid workspace")

type topupRequest struct {
	StripeTopupInput
	BytesAmount int64
}

func paidTopupWorkspace(w Workspace, at time.Time) bool {
	return w.ProductCode != "" && w.CustomerID != "" && w.PeriodStart != nil && !at.Before(*w.PeriodStart) && w.PaidBenefitsUntil != nil && at.Before(*w.PaidBenefitsUntil)
}

// Topup journals the immutable provider request before creating payment Checkout.
// The redirect is only a receipt; only a verified paid webhook grants bytes.
func (s *WorkspaceService) Topup(ctx context.Context, account, team, code, key, success, cancel string, version int64) (CheckoutSession, error) {
	provider, ok := s.stripe.(StripeTopupClient)
	if !ok {
		return CheckoutSession{}, ErrStripeUnavailable
	}
	if s.key == "" {
		return CheckoutSession{}, ErrWorkspaceEncryptionUnavailable
	}
	hash, err := workspaceRequestHash(struct {
		Account, Team, Code, Success, Cancel string
		Version                              int64
	}{account, team, code, success, cancel, version})
	if err != nil {
		return CheckoutSession{}, err
	}
	request, replay, err := s.prepareTopup(ctx, account, team, code, key, success, cancel, version, hash)
	if err != nil {
		return CheckoutSession{}, err
	}
	if replay != nil {
		state, e := provider.GetTopupCheckout(ctx, replay.ProviderSessionID)
		if e != nil {
			return CheckoutSession{}, e
		}
		if !topupIdentity(state, request) || state.ID != replay.ProviderSessionID {
			return CheckoutSession{}, ErrProviderOutcomeUnknown
		}
		if state.Status == "expired" {
			return CheckoutSession{}, ErrCheckoutSessionExpired
		}
		if state.Status != "open" {
			return CheckoutSession{}, ErrCheckoutPending
		}
		if err = s.reauthorizeBilling(ctx, account, team); err != nil {
			return CheckoutSession{}, err
		}
		return *replay, nil
	}
	if _, err = s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET provider_attempted_at=coalesce(provider_attempted_at,now()) WHERE idempotency_key=$1 AND state='pending'`, key); err != nil {
		return CheckoutSession{}, err
	}
	result, err := provider.CreateTopupCheckout(ctx, request.StripeTopupInput)
	if err != nil {
		return CheckoutSession{}, s.failOperation(ctx, key, err)
	}
	if result.ProviderSessionID == "" || result.URL == "" || result.ProviderCustomerID != request.CustomerID {
		return CheckoutSession{}, s.failOperation(ctx, key, ErrProviderOutcomeUnknown)
	}
	if err = s.completeOperation(ctx, key, result); err != nil {
		_ = s.markOperation(ctx, key, "uncertain")
		return CheckoutSession{}, errors.Join(ErrProviderOutcomeUnknown, err)
	}
	if err = s.reauthorizeBilling(ctx, account, team); err != nil {
		return CheckoutSession{}, err
	}
	return result, nil
}
func (s *WorkspaceService) prepareTopup(ctx context.Context, account, team, code, key, success, cancel string, version int64, hash string) (topupRequest, *CheckoutSession, error) {
	var request topupRequest
	var replay *CheckoutSession
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		w, e := WorkspaceTx(ctx, tx, account, team, true)
		if e != nil {
			return e
		}
		if !paidTopupWorkspace(w, time.Now().UTC()) {
			return ErrTopupRequiresPaid
		}
		op, found, e := loadWorkspaceOperation(ctx, tx, key)
		if e != nil {
			return e
		}
		if found {
			if op.WorkspaceID != w.ID || op.Actor != account || op.Kind != "topup" || op.Hash != hash {
				return ErrIdempotencyConflict
			}
			if e = s.decryptResult(op.RequestCiphertext, &request); e != nil {
				return e
			}
			switch op.State {
			case "succeeded":
				var result CheckoutSession
				if e = s.decryptResult(op.Ciphertext, &result); e != nil {
					return e
				}
				replay = &result
				return nil
			case "failed":
				return ErrWorkspaceOperationFailed
			case "pending":
				if time.Since(op.UpdatedAt) < time.Minute {
					return ErrCheckoutPending
				}
			}
			if op.ProviderAttemptedAt.Valid && time.Since(op.ProviderAttemptedAt.Time) >= 24*time.Hour {
				return ErrStripeOperationExpired
			}
			_, e = tx.Exec(ctx, `UPDATE billing_workspace_operations SET state='pending',updated_at=now() WHERE idempotency_key=$1`, key)
			return e
		}
		var pending bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_workspace_operations WHERE workspace_id=$1 AND kind='topup' AND state IN ('pending','uncertain'))`, w.ID).Scan(&pending); e != nil {
			return e
		}
		if pending {
			return ErrCheckoutPending
		}
		var metadata string
		var currentVersion int64
		e = tx.QueryRow(ctx, `SELECT provider_price_id,catalog_ref,version FROM billing_products WHERE code=$1 AND provider='stripe' AND active AND catalog_type='top_up' AND verified_unit_amount>0 AND verified_currency IS NOT NULL AND verified_interval='one_time'`, code).Scan(&request.PriceID, &metadata, &currentVersion)
		if e == pgx.ErrNoRows {
			return ErrUnknownProduct
		}
		if e != nil {
			return e
		}
		if currentVersion != version {
			return ErrProductChanged
		}
		var p Product
		if e = parseProductMetadata(metadata, &p); e != nil {
			return e
		}
		if p.BytesAmount == nil || *p.BytesAmount <= 0 {
			return ErrUnknownProduct
		}
		request.BytesAmount = *p.BytesAmount
		request.StripeTopupInput = StripeTopupInput{CustomerID: w.CustomerID, WorkspaceID: w.ID, PriceID: request.PriceID, IdempotencyKey: key, SuccessURL: success, CancelURL: cancel}
		cipher, e := s.encryptResult(request)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext) VALUES($1,$2,$3,'topup',$4,'pending',$5)`, key, w.ID, account, hash, cipher)
		return e
	})
	return request, replay, err
}

func topupIdentity(state StripeTopupState, r topupRequest) bool {
	return state.Mode == "payment" && state.CustomerID == r.CustomerID && state.WorkspaceID == r.WorkspaceID && state.Operation == r.IdempotencyKey && state.PriceID == r.PriceID && state.Quantity == 1
}

// WorkspaceTopupRemainingTx uses the accounting instant, not wall-clock time.
// Callers must establish paid eligibility before using this ledger for admission.
func WorkspaceTopupRemainingTx(ctx context.Context, tx *db.Tx, workspaceID string, at time.Time) (int64, error) {
	if tx == nil || workspaceID == "" || at.IsZero() {
		return 0, ErrBillingPolicyUnconfigured
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM billing_workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(new(string)); err != nil {
		return 0, err
	}
	var total int64
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(remaining_bytes),0) FROM billing_workspace_topups WHERE workspace_id=$1 AND purchased_at<=$2 AND expires_at>$2`, workspaceID, at).Scan(&total)
	return total, err
}
func ConsumeWorkspaceTopupsTx(ctx context.Context, tx *db.Tx, workspaceID string, bytes int64, at time.Time) (int64, error) {
	if bytes < 0 {
		return 0, ErrBillingPolicyUnconfigured
	}
	if _, err := WorkspaceTopupRemainingTx(ctx, tx, workspaceID, at); err != nil {
		return 0, err
	}
	// A single statement drains the oldest expiry first, bounded by the request.
	var consumed int64
	err := tx.QueryRow(ctx, `WITH eligible AS (
 SELECT provider_session_id,remaining_bytes,coalesce(sum(remaining_bytes) OVER (ORDER BY expires_at,purchased_at,provider_session_id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0) AS preceding
 FROM billing_workspace_topups WHERE workspace_id=$1 AND purchased_at<=$3 AND expires_at>$3 AND remaining_bytes>0
 ), debited AS (UPDATE billing_workspace_topups t SET remaining_bytes=t.remaining_bytes-least(e.remaining_bytes,greatest(0,$2::bigint-e.preceding)) FROM eligible e WHERE t.provider_session_id=e.provider_session_id
 RETURNING least(e.remaining_bytes,greatest(0,$2::bigint-e.preceding)) AS bytes)
 SELECT coalesce(sum(bytes),0) FROM debited`, workspaceID, bytes, at).Scan(&consumed)
	return consumed, err
}
