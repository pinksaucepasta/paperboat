package billing

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
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

// PreviewSeats runs its provider calls outside any transaction: the transaction would hold teams.Lock (every team write
// on the platform waits) for up to the provider timeout, and InTx replays its closure on a conflict.
// Prior art: NetBird management (account manager) holds only account-scoped row locks and makes no
// provider calls under them; Teleport lib/usagereporter submits outside any backend lock.
func (s *WorkspaceService) PreviewSeats(ctx context.Context, account, team string, quantity int) (SeatPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	provider, ok := s.stripe.(SeatProvider)
	if !ok {
		return SeatPreview{}, ErrStripeUnavailable
	}
	var in SeatChange
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		_, in, err = seatChangeTx(ctx, tx, account, team, quantity)
		return err
	})
	if err != nil {
		return SeatPreview{}, err
	}
	return provider.PreviewSeats(ctx, in)
}

func (s *WorkspaceService) ChangeSeats(ctx context.Context, account, team string, quantity int, key string) (SeatPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	provider, ok := s.stripe.(SeatProvider)
	if !ok {
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
	var first seatIntent
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var e error
		first, e = s.seatIntentTx(ctx, tx, account, team, quantity, key, hash)
		return e
	})
	if err != nil || first.replay != nil {
		return derefSeatPreview(first.replay), err
	}
	if first.started {
		return s.applySeatOperation(ctx, key)
	}
	in := first.change
	if in.Preview, err = provider.PreviewSeats(ctx, in); err != nil {
		return SeatPreview{}, err
	}
	var replay *SeatPreview
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		again, e := s.seatIntentTx(ctx, tx, account, team, quantity, key, hash)
		if e != nil {
			return e
		}
		if again.replay != nil || again.started {
			// Another request with this key recorded the operation while Stripe priced it.
			replay = again.replay
			return nil
		}
		if again.workspaceID != first.workspaceID || !sameSeatChange(again.change, in) {
			// The subscription changed while Stripe priced it; nothing was recorded, the same ID retries.
			return ErrCheckoutPending
		}
		cipher, e := s.encryptResult(in)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext) VALUES($1,$2,$3,'seats',$4,'pending',$5)`, key, first.workspaceID, account, hash, cipher); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE billing_workspaces SET pending_seat_capacity=$2 WHERE id=$1`, first.workspaceID, quantity)
		return e
	})
	if err != nil || replay != nil {
		return derefSeatPreview(replay), err
	}
	return s.applySeatOperation(ctx, key)
}

// sameSeatChange compares everything Stripe priced except the proration instant and the preview itself.
func sameSeatChange(a, b SeatChange) bool {
	return a.TeamID == b.TeamID && a.SubscriptionID == b.SubscriptionID && a.CustomerID == b.CustomerID &&
		a.ItemID == b.ItemID && a.PriceID == b.PriceID && a.Quantity == b.Quantity &&
		a.PreviousQuantity == b.PreviousQuantity && a.HadScheduledReduction == b.HadScheduledReduction &&
		a.PeriodStart.Equal(b.PeriodStart) && a.PeriodEnd.Equal(b.PeriodEnd)
}

func derefSeatPreview(p *SeatPreview) SeatPreview {
	if p == nil {
		return SeatPreview{}
	}
	return *p
}

// seatIntent is a stored result (replay), an existing unfinished operation for this key (started: the
// caller only re-applies it), or the change to price for the workspace.
type seatIntent struct {
	replay      *SeatPreview
	started     bool
	change      SeatChange
	workspaceID string
}

func (s *WorkspaceService) seatIntentTx(ctx context.Context, tx *db.Tx, account, team string, quantity int, key, hash string) (seatIntent, error) {
	w, e := WorkspaceTx(ctx, tx, account, team, true)
	if e != nil {
		return seatIntent{}, e
	}
	op, found, e := loadWorkspaceOperation(ctx, tx, key)
	if e != nil {
		return seatIntent{}, e
	}
	if found {
		if op.WorkspaceID != w.ID || op.Actor != account || op.Kind != "seats" || op.Hash != hash {
			return seatIntent{}, ErrIdempotencyConflict
		}
		switch op.State {
		case "succeeded":
			var out SeatPreview
			if e = s.decryptResult(op.Ciphertext, &out); e != nil {
				return seatIntent{}, e
			}
			return seatIntent{replay: &out}, nil
		case "failed":
			return seatIntent{}, ErrWorkspaceOperationFailed
		}
		return seatIntent{started: true}, nil
	}
	w, in, e := seatChangeTx(ctx, tx, account, team, quantity)
	if e != nil {
		return seatIntent{}, e
	}
	var busy bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_workspace_operations WHERE workspace_id=$1
 AND kind='seats' AND state IN ('pending','uncertain'))`, w.ID).Scan(&busy); e != nil {
		return seatIntent{}, e
	}
	if busy {
		return seatIntent{}, ErrCheckoutPending
	}
	return seatIntent{change: in, workspaceID: w.ID}, nil
}

// applySeatOperation commits intent before the provider call; a process exit or ambiguous response replays
// the exact encrypted provider request and idempotency key, never membership-derived quantity. The
// committed pending_seat_capacity bounds membership while the provider call runs outside the lock.
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
	var done bool
	var in SeatChange
	var workspaceID string
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		done, in, workspaceID, err = s.seatOperationTx(ctx, tx, key, &out)
		return err
	})
	if err != nil || done {
		return out, err
	}
	out, providerErr := provider.ChangeSeats(ctx, in, key)
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		op, found, err := loadWorkspaceOperation(ctx, tx, key)
		if err != nil {
			return err
		}
		if !found || (op.State != "pending" && op.State != "uncertain") {
			// A concurrent replay of the same key already recorded the outcome.
			return nil
		}
		if providerErr == nil {
			return s.completeSeatOperationTx(ctx, tx, key, workspaceID, in, out)
		}
		state := "failed"
		if stripeOutcomeUncertain(providerErr) {
			state = "uncertain"
		}
		_, err = tx.Exec(ctx, `UPDATE billing_workspace_operations SET state=$2,provider_attempted_at=coalesce(provider_attempted_at,now()),updated_at=now() WHERE idempotency_key=$1`, key, state)
		if err == nil && state == "failed" {
			_, err = tx.Exec(ctx, `UPDATE billing_workspaces SET pending_seat_capacity=NULL WHERE id=$1`, workspaceID)
		}
		return err
	})
	if providerErr != nil {
		if stripeOutcomeUncertain(providerErr) {
			return SeatPreview{}, errors.Join(ErrProviderOutcomeUnknown, providerErr)
		}
		return SeatPreview{}, errors.Join(providerErr, err)
	}
	if err != nil {
		// Stripe applied the change but recording failed: the outcome is known to Stripe only, and
		// a retry with the same key replays the idempotent request and records it.
		return SeatPreview{}, errors.Join(ErrProviderOutcomeUnknown, err)
	}
	return out, nil
}

// seatOperationTx loads the operation under the team lock. done=true means it already finished (out holds
// a success); otherwise it returns the validated provider request.
func (s *WorkspaceService) seatOperationTx(ctx context.Context, tx *db.Tx, key string, out *SeatPreview) (bool, SeatChange, string, error) {
	if err := teams.Lock(ctx, tx); err != nil {
		return false, SeatChange{}, "", err
	}
	op, found, err := loadWorkspaceOperation(ctx, tx, key)
	if err != nil {
		return false, SeatChange{}, "", err
	}
	if !found || op.Kind != "seats" {
		return false, SeatChange{}, "", ErrIdempotencyConflict
	}
	if op.State == "succeeded" {
		return true, SeatChange{}, "", s.decryptResult(op.Ciphertext, out)
	}
	if op.State == "failed" {
		return false, SeatChange{}, "", ErrWorkspaceOperationFailed
	}
	if op.ProviderAttemptedAt.Valid && time.Since(op.ProviderAttemptedAt.Time) >= 24*time.Hour {
		return false, SeatChange{}, "", ErrStripeOperationExpired
	}
	var in SeatChange
	if err = s.decryptResult(op.RequestCiphertext, &in); err != nil {
		return false, SeatChange{}, "", err
	}
	// Old auto-membership operations require operator reconciliation, never replay.
	if in.TeamID == "" || in.PreviousQuantity < 1 || !in.PeriodEnd.After(time.Now()) {
		return false, SeatChange{}, "", ErrStripeOperationExpired
	}
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_workspaces w
 JOIN billing_products p ON p.code=w.product_code AND p.provider='stripe'
 WHERE w.id=$1 AND w.provider_subscription_id=$2 
 AND w.provider_customer_id=$3 AND w.provider_item_id=$4 
 AND p.provider_price_id=$5 AND w.period_start=$6 AND w.period_end=$7)`, op.WorkspaceID, in.SubscriptionID, in.CustomerID, in.ItemID, in.PriceID, in.PeriodStart, in.PeriodEnd).Scan(&valid); err != nil {
		return false, SeatChange{}, "", err
	}
	if !valid {
		return false, SeatChange{}, "", ErrProviderOutcomeUnknown
	}
	used, err := teams.SeatUsageTx(ctx, tx, in.TeamID)
	if err != nil {
		return false, SeatChange{}, "", err
	}
	if used > in.Quantity {
		return false, SeatChange{}, "", ErrSeatQuantity
	}
	return false, in, op.WorkspaceID, nil
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
