package billing

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

var ErrTeamCreationInvalid = errors.New("invalid paid team creation request")
var ErrTeamReserved = errors.New("team identifier is already reserved")
var teamSlug = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

type TeamCreationInput struct {
	OperationID string `json:"operation_id"`
	TeamID      string `json:"team_id"`
	Seats       int    `json:"seats"`
	Interval    string `json:"interval"`
	SuccessURL  string `json:"success_url"`
	CancelURL   string `json:"cancel_url"`
}

type teamCreationRequest struct {
	Input                                   TeamCreationInput
	Email, PriceID, ProductCode, CustomerID string
}

func activeTeamCreator(ctx context.Context, tx *db.Tx, actor string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`, actor).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrWorkspaceForbidden
	}
	return nil
}

// CreatePaidTeam reserves identity without granting membership or workspace access.
// The persisted encrypted request freezes catalog and redirects for provider retries.
func (s *WorkspaceService) CreatePaidTeam(ctx context.Context, actor, email string, in TeamCreationInput) (CheckoutSession, error) {
	if in.OperationID == "" || len(in.OperationID) > 200 || len(in.TeamID) > 63 || !teamSlug.MatchString(in.TeamID) || in.TeamID == "personal" || in.Seats < 1 || in.Seats > teams.MaximumMembers || (in.Interval != "month" && in.Interval != "year") || in.SuccessURL == "" || in.CancelURL == "" {
		return CheckoutSession{}, ErrTeamCreationInvalid
	}
	if s.stripe == nil {
		return CheckoutSession{}, ErrStripeUnavailable
	}
	if s.key == "" {
		return CheckoutSession{}, ErrWorkspaceEncryptionUnavailable
	}
	hash, err := workspaceRequestHash(struct {
		Actor string
		Input TeamCreationInput
	}{actor, in})
	if err != nil {
		return CheckoutSession{}, err
	}
	req := teamCreationRequest{Input: in, Email: email, ProductCode: "team-" + in.Interval}
	var replay *CheckoutSession
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		if err := activeTeamCreator(ctx, tx, actor); err != nil {
			return err
		}
		var owner, oldHash, state string
		var request, result []byte
		var attempted, updated time.Time
		err := tx.QueryRow(ctx, `SELECT actor_user_id,request_hash,state,request_ciphertext,result_ciphertext,attempted_at,updated_at FROM billing_team_orders WHERE operation_id=$1 FOR UPDATE`, in.OperationID).Scan(&owner, &oldHash, &state, &request, &result, &attempted, &updated)
		if err == nil {
			if owner != actor || oldHash != hash {
				return ErrIdempotencyConflict
			}
			if err = s.decryptResult(request, &req); err != nil {
				return err
			}
			if state == "failed" {
				return ErrWorkspaceOperationFailed
			}
			if len(result) > 0 {
				var v CheckoutSession
				if err = s.decryptResult(result, &v); err != nil {
					return err
				}
				replay = &v
				return nil
			}
			if time.Since(attempted) >= 24*time.Hour {
				return ErrStripeOperationExpired
			}
			if time.Since(updated) < time.Minute {
				return ErrCheckoutPending
			}
			_, err = tx.Exec(ctx, `UPDATE billing_team_orders SET updated_at=now() WHERE operation_id=$1`, in.OperationID)
			return err
		}
		if err != pgx.ErrNoRows {
			return err
		}
		var taken bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams WHERE team_id=$1) OR EXISTS(SELECT 1 FROM billing_team_orders WHERE team_id=$1 AND state<>'failed')`, in.TeamID).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return ErrTeamReserved
		}
		if err = tx.QueryRow(ctx, `SELECT provider_price_id FROM billing_products WHERE code=$1 AND provider='stripe' AND active AND catalog_type='team_subscription' AND verified_unit_amount IS NOT NULL AND verified_currency IS NOT NULL AND verified_interval=$2`, req.ProductCode, in.Interval).Scan(&req.PriceID); err != nil {
			if err == pgx.ErrNoRows {
				return ErrUnknownProduct
			}
			return err
		}
		cipher, err := s.encryptResult(req)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO billing_team_orders(operation_id,team_id,actor_user_id,request_hash,request_ciphertext) VALUES($1,$2,$3,$4,$5)`, in.OperationID, in.TeamID, actor, hash, cipher)
		return err
	})
	if err != nil {
		return CheckoutSession{}, err
	}
	if replay != nil {
		return s.replayTeamCheckout(ctx, req, *replay)
	}
	return s.startTeamCheckout(ctx, actor, req)
}

func (s *WorkspaceService) startTeamCheckout(ctx context.Context, actor string, req teamCreationRequest) (CheckoutSession, error) {
	key := req.Input.OperationID
	fail := func(err error) (CheckoutSession, error) {
		// Definitive provider rejection releases the slug, but keeps the exact
		// operation receipt terminal. Unknown outcomes retain their reservation.
		if !stripeOutcomeUncertain(err) {
			if _, saveErr := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_team_orders SET state='failed',updated_at=now() WHERE operation_id=$1 AND state='pending'`, key); saveErr != nil {
				return CheckoutSession{}, errors.Join(err, saveErr)
			}
			return CheckoutSession{}, errors.Join(ErrWorkspaceOperationFailed, err)
		}
		_, _ = s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_team_orders SET updated_at=now()-interval '1 minute' WHERE operation_id=$1`, key)
		return CheckoutSession{}, errors.Join(ErrProviderOutcomeUnknown, err)
	}
	if req.CustomerID == "" {
		customer, err := s.stripe.CreateCustomer(ctx, "team:"+req.Input.TeamID, req.Email, "team-create:"+key+":customer")
		if err != nil {
			return fail(err)
		}
		req.CustomerID = customer
		cipher, err := s.encryptResult(req)
		if err != nil {
			return fail(err)
		}
		if _, err = s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_team_orders SET request_ciphertext=$2,provider_customer_id=$3 WHERE operation_id=$1`, key, cipher, customer); err != nil {
			return fail(err)
		}
	}
	session, err := s.stripe.CreateSubscriptionCheckout(ctx, StripeCheckoutInput{CustomerID: req.CustomerID, PriceID: req.PriceID, Quantity: req.Input.Seats, SuccessURL: req.Input.SuccessURL, CancelURL: req.Input.CancelURL, IdempotencyKey: "team-create:" + key})
	if err != nil {
		return fail(err)
	}
	if session.ProviderSessionID == "" || session.ProviderCustomerID != req.CustomerID || session.URL == "" {
		return fail(ErrProviderOutcomeUnknown)
	}
	cipher, err := s.encryptResult(session)
	if err != nil {
		return fail(err)
	}
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := activeTeamCreator(ctx, tx, actor); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE billing_team_orders SET result_ciphertext=$2,provider_session_id=$3,state=CASE WHEN state='paid' THEN state ELSE 'checkout' END,updated_at=now() WHERE operation_id=$1`, key, cipher, session.ProviderSessionID)
		return err
	})
	if err != nil {
		return fail(err)
	}
	return session, nil
}

func (s *WorkspaceService) replayTeamCheckout(ctx context.Context, req teamCreationRequest, receipt CheckoutSession) (CheckoutSession, error) {
	current, err := s.stripe.GetCheckoutSession(ctx, receipt.ProviderSessionID)
	if err != nil {
		return CheckoutSession{}, err
	}
	if current.ID != receipt.ProviderSessionID || current.CustomerID != req.CustomerID || current.CheckoutOperation != "team-create:"+req.Input.OperationID || current.Mode != "subscription" || current.PriceID != req.PriceID || current.Quantity != req.Input.Seats {
		return CheckoutSession{}, ErrProviderOutcomeUnknown
	}
	if current.Status == "expired" {
		if err = s.expireTeamOrder(ctx, req.Input.OperationID); err != nil {
			return CheckoutSession{}, err
		}
		return CheckoutSession{}, ErrCheckoutSessionExpired
	}
	if current.Status != "open" {
		return CheckoutSession{}, ErrCheckoutPending
	}
	if current.URL != "" {
		receipt.URL = current.URL
	}
	return receipt, nil
}

func (s *WorkspaceService) expireTeamOrder(ctx context.Context, key string) error {
	_, err := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_team_orders SET state='failed',updated_at=now() WHERE operation_id=$1 AND state IN ('pending','checkout')`, key)
	return err
}
