package billing

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

var ErrSeatQuantity = errors.New("seat quantity must cover members and pending invitations")
var ErrSeatSubscription = errors.New("an active team subscription is required to change seats")

// SeatProvider keeps the purchased-seat contract separate from checkout/portal.
type SeatProvider interface {
	PreviewSeats(context.Context, SeatChange) (SeatPreview, error)
	ChangeSeats(context.Context, SeatChange, string) (SeatPreview, error)
}

type SeatPreview struct {
	AmountDueNow      int64     `json:"amount_due_now"`
	Currency          string    `json:"currency"`
	NextInvoiceAmount int64     `json:"next_invoice_amount"`
	NextInvoiceAt     time.Time `json:"next_invoice_at"`
}

type SeatChange struct {
	TeamID, SubscriptionID, CustomerID, ItemID, PriceID string
	Quantity, PreviousQuantity                          int
	HadScheduledReduction                               bool
	PeriodStart, PeriodEnd                              time.Time
	ProrationAt                                         int64
	Preview                                             SeatPreview
}

func seatChangeTx(ctx context.Context, tx *db.Tx, account, team string, quantity int) (Workspace, SeatChange, error) {
	w, err := WorkspaceTx(ctx, tx, account, team, true)
	if err != nil {
		return w, SeatChange{}, err
	}
	if team == "" || w.SubscriptionID == "" || w.ItemID == "" || (w.State != "active" && w.State != "trialing") || w.CancelAtPeriodEnd || w.PeriodStart == nil || w.PeriodEnd == nil || !w.PeriodEnd.After(time.Now()) {
		return w, SeatChange{}, ErrSeatSubscription
	}
	if quantity < 1 || quantity > teams.MaximumMembers || quantity < w.SeatsUsed {
		return w, SeatChange{}, ErrSeatQuantity
	}
	var price string
	if err = tx.QueryRow(ctx, `SELECT provider_price_id FROM billing_products WHERE code=$1 AND provider='stripe'`, w.ProductCode).Scan(&price); err != nil {
		return w, SeatChange{}, err
	}
	return w, SeatChange{TeamID: w.TeamID, SubscriptionID: w.SubscriptionID, CustomerID: w.CustomerID, ItemID: w.ItemID, PriceID: price, Quantity: quantity, PreviousQuantity: w.Quantity, PeriodStart: *w.PeriodStart, PeriodEnd: *w.PeriodEnd, ProrationAt: time.Now().Unix(), HadScheduledReduction: w.ScheduledQuantity != nil}, nil
}

func (s *WorkspaceService) PreviewSeats(ctx context.Context, account, team string, quantity int) (SeatPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	provider, ok := s.stripe.(SeatProvider)
	if !ok {
		return SeatPreview{}, ErrStripeUnavailable
	}
	var result SeatPreview
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, in, err := seatChangeTx(ctx, tx, account, team, quantity)
		if err != nil {
			return err
		}
		result, err = provider.PreviewSeats(ctx, in)
		return err
	})
	return result, err
}

func (s *WorkspaceService) ChangeSeats(ctx context.Context, account, team string, quantity int, key string) (SeatPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if _, ok := s.stripe.(SeatProvider); !ok {
		return SeatPreview{}, ErrStripeUnavailable
	}
	if s.key == "" {
		return SeatPreview{}, ErrWorkspaceEncryptionUnavailable
	}
	if key == "" || len(key) > 200 {
		return SeatPreview{}, ErrIdempotencyConflict
	}
	hash, err := workspaceRequestHash(struct {
		Account, Team string
		Quantity      int
	}{account, team, quantity})
	if err != nil {
		return SeatPreview{}, err
	}
	var replay *SeatPreview
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		w, e := WorkspaceTx(ctx, tx, account, team, true)
		if e != nil {
			return e
		}
		op, found, e := loadWorkspaceOperation(ctx, tx, key)
		if e != nil {
			return e
		}
		if found {
			if op.WorkspaceID != w.ID || op.Actor != account || op.Kind != "seats" || op.Hash != hash {
				return ErrIdempotencyConflict
			}
			if op.State == "succeeded" {
				var out SeatPreview
				if e = s.decryptResult(op.Ciphertext, &out); e != nil {
					return e
				}
				replay = &out
				return nil
			}
			if op.State == "failed" {
				return ErrWorkspaceOperationFailed
			}
			return nil
		}
		w, in, e := seatChangeTx(ctx, tx, account, team, quantity)
		if e != nil {
			return e
		}
		var busy bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_workspace_operations WHERE workspace_id=$1
 AND kind='seats' AND state IN ('pending','uncertain'))`, w.ID).Scan(&busy); e != nil {
			return e
		}
		if busy {
			return ErrCheckoutPending
		}
		in.Preview, e = s.stripe.(SeatProvider).PreviewSeats(ctx, in)
		if e != nil {
			return e
		}
		cipher, e := s.encryptResult(in)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext) VALUES($1,$2,$3,'seats',$4,'pending',$5)`, key, w.ID, account, hash, cipher)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE billing_workspaces SET pending_seat_capacity=$2 WHERE id=$1`, w.ID, quantity)
		return e
	})
	if err != nil {
		return SeatPreview{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	return s.applySeatOperation(ctx, key)
}

// applySeatOperation holds the same bounded transaction lock as membership changes.
// Intent is committed first; a process exit or ambiguous response replays the exact
// encrypted provider request and idempotency keys, never membership-derived quantity.
func (s *WorkspaceService) applySeatOperation(ctx context.Context, key string) (SeatPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	provider, ok := s.stripe.(SeatProvider)
	if !ok {
		return SeatPreview{}, ErrStripeUnavailable
	}
	if _, err := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET provider_attempted_at=coalesce(provider_attempted_at,now())
 WHERE idempotency_key=$1 AND state IN ('pending','uncertain')`, key); err != nil {
		return SeatPreview{}, err
	}
	var out SeatPreview
	var providerErr error
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		op, found, err := loadWorkspaceOperation(ctx, tx, key)
		if err != nil {
			return err
		}
		if !found || op.Kind != "seats" {
			return ErrIdempotencyConflict
		}
		if op.State == "succeeded" {
			return s.decryptResult(op.Ciphertext, &out)
		}
		if op.State == "failed" {
			return ErrWorkspaceOperationFailed
		}
		if op.ProviderAttemptedAt.Valid && time.Since(op.ProviderAttemptedAt.Time) >= 24*time.Hour {
			return ErrStripeOperationExpired
		}
		var in SeatChange
		if err = s.decryptResult(op.RequestCiphertext, &in); err != nil {
			return err
		}
		// Old auto-membership operations require operator reconciliation, never replay.
		if in.TeamID == "" || in.PreviousQuantity < 1 || !in.PeriodEnd.After(time.Now()) {
			return ErrStripeOperationExpired
		}
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_workspaces w
 JOIN billing_products p ON p.code=w.product_code AND p.provider='stripe'
 WHERE w.id=$1 AND w.provider_subscription_id=$2 
 AND w.provider_customer_id=$3 AND w.provider_item_id=$4 
 AND p.provider_price_id=$5 AND w.period_start=$6 AND w.period_end=$7)`, op.WorkspaceID, in.SubscriptionID, in.CustomerID, in.ItemID, in.PriceID, in.PeriodStart, in.PeriodEnd).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrProviderOutcomeUnknown
		}
		used, err := teams.SeatUsageTx(ctx, tx, in.TeamID)
		if err != nil {
			return err
		}
		if used > in.Quantity {
			return ErrSeatQuantity
		}
		out, providerErr = provider.ChangeSeats(ctx, in, key)
		if providerErr != nil {
			state := "failed"
			if stripeOutcomeUncertain(providerErr) {
				state = "uncertain"
			}
			_, err = tx.Exec(ctx, `UPDATE billing_workspace_operations SET state=$2,provider_attempted_at=coalesce(provider_attempted_at,now()),updated_at=now() WHERE idempotency_key=$1`, key, state)
			if err == nil && state == "failed" {
				_, err = tx.Exec(ctx, `UPDATE billing_workspaces SET pending_seat_capacity=NULL WHERE id=$1`, op.WorkspaceID)
			}
			return err
		}
		return s.completeSeatOperationTx(ctx, tx, key, op.WorkspaceID, in, out)
	})
	if err != nil {
		return SeatPreview{}, err
	}
	if providerErr != nil {
		if stripeOutcomeUncertain(providerErr) {
			return SeatPreview{}, errors.Join(ErrProviderOutcomeUnknown, providerErr)
		}
		return SeatPreview{}, providerErr
	}
	return out, nil
}

func (s *WorkspaceService) completeSeatOperationTx(ctx context.Context, tx *db.Tx, key, workspaceID string, in SeatChange, out SeatPreview) error {
	cipher, err := s.encryptResult(out)
	if err != nil {
		return err
	}
	if in.Quantity < in.PreviousQuantity {
		_, err = tx.Exec(ctx, `UPDATE billing_workspaces SET pending_seat_capacity=NULL,scheduled_quantity=$2,scheduled_quantity_at=$3,revision=revision+1,updated_at=now() WHERE id=$1 AND provider_subscription_id=$4`, workspaceID, in.Quantity, in.PeriodEnd, in.SubscriptionID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE billing_workspaces SET pending_seat_capacity=NULL,quantity=$2,scheduled_quantity=NULL,scheduled_quantity_at=NULL,revision=revision+1,updated_at=now() WHERE id=$1 AND provider_subscription_id=$3`, workspaceID, in.Quantity, in.SubscriptionID)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE billing_workspace_operations SET state='succeeded',result_ciphertext=$2,updated_at=now() WHERE idempotency_key=$1`, key, cipher)
	return err
}

// ReconcileMemberships recovers explicit seat purchases only. Unused paid seats
// belong to the owner and are never reduced by this worker.
func (s *WorkspaceService) ReconcileMemberships(ctx context.Context, limit int) error {
	if limit <= 0 {
		return nil
	}
	if limit > 128 {
		limit = 128
	}
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT idempotency_key FROM paperboat.billing_workspace_operations WHERE kind='seats' AND state IN ('pending','uncertain')
 ORDER BY updated_at LIMIT $1`, limit)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, key := range keys {
		if _, err = s.applySeatOperation(ctx, key); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
