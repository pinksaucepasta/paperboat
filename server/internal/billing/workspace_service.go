package billing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"hash/fnv"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

type WorkspaceService struct {
	store            *db.DB
	stripe           StripeClient
	key              string
	topupMaxQuantity int
	portals          PortalConfigurations
	mail             config.TransactionalMail
	mailEnabled      bool
	teamTrialDays    int
}

var ErrProductChanged = errors.New("billing product price changed")

var ErrInvalidProductPage = errors.New("invalid billing product page")

var ErrStripeUnavailable = errors.New("stripe billing provider is not configured")
var ErrWorkspaceEncryptionUnavailable = errors.New("workspace billing encryption key is not configured")
var ErrWorkspaceOperationFailed = errors.New("workspace billing operation definitively failed")
var ErrStripeOperationExpired = errors.New("stripe operation requires operator reconciliation after idempotency expiry")
var ErrWorkspaceSubscriptionExists = errors.New("workspace already has a subscription")
var ErrCheckoutSessionExpired = errors.New("checkout session expired")

type WorkspaceProduct struct {
	Code          string `json:"code"`
	Name          string `json:"name"`
	CatalogType   string `json:"catalog_type"`
	UnitAmount    *int64 `json:"unit_amount"`
	Currency      string `json:"currency"`
	Interval      string `json:"interval"`
	PriceVerified bool   `json:"price_verified"`
	Version       int64  `json:"version"`
	UnitBytes     *int64 `json:"unit_bytes"`
	MaxQuantity   *int   `json:"max_quantity"`
}

type PortalConfigurations struct{ Personal, Team string }

func NewWorkspaceService(store *db.DB, stripe StripeClient, key string, portals ...PortalConfigurations) *WorkspaceService {
	s := &WorkspaceService{store: store, stripe: stripe, key: key, topupMaxQuantity: config.Default().Billing.TopupMaxQuantity}
	for _, configuration := range portals {
		s.portals = configuration
	}
	return s
}

const staleProviderReadWindow = 5 * time.Minute

func (s *WorkspaceService) Overview(ctx context.Context, account, team string) (Workspace, error) {
	var out Workspace
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		out, err = WorkspaceTx(ctx, tx, account, team, false)
		if err != nil {
			return err
		}
		if out.ProductCode != "" {
			p, priceErr := workspaceProduct(tx.QueryRow(ctx, `SELECT code,catalog_ref,verified_unit_amount,verified_currency,verified_interval,version,catalog_type FROM billing_products WHERE code=$1 AND provider='stripe'`, out.ProductCode))
			if priceErr != nil && priceErr != pgx.ErrNoRows {
				return priceErr
			}
			if priceErr == nil {
				out.Product = &p
			}
		}
		out.TopupBytesRemaining, err = WorkspaceTopupRemainingTx(ctx, tx, out.ID, time.Now().UTC())
		if err != nil {
			return err
		}
		if out.Limits, err = planlimits.UsageTx(ctx, tx, planlimits.Scope{Account: account, Team: team}); err != nil {
			return err
		}
		if team == "" {
			days, err := s.teamTrialDaysTx(ctx, tx, account)
			if err != nil {
				return err
			}
			out.TeamTrialDays = &days
		}
		return s.usageOverviewTx(ctx, tx, &out)
	})
	return out, err
}

type WorkspaceProductPage struct {
	Items []WorkspaceProduct `json:"items"`
	Total int                `json:"-"`
}

func workspaceProduct(row interface{ Scan(...any) error }) (WorkspaceProduct, error) {
	var p WorkspaceProduct
	var metadata string
	var amount sql.NullInt64
	var currency, interval sql.NullString
	if err := row.Scan(&p.Code, &metadata, &amount, &currency, &interval, &p.Version, &p.CatalogType); err != nil {
		return p, err
	}
	var product Product
	if err := parseProductMetadata(metadata, &product); err != nil {
		return p, err
	}
	p.UnitBytes = product.BytesAmount
	p.Name = product.DisplayName
	if p.Name == "" {
		p.Name = p.Code
	}
	p.PriceVerified = amount.Valid && currency.Valid && interval.Valid
	if p.PriceVerified {
		p.UnitAmount = &amount.Int64
		p.Currency = currency.String
		p.Interval = interval.String
	}
	return p, nil
}

func (s *WorkspaceService) Products(ctx context.Context, account, team string, limit, offset int) (WorkspaceProductPage, error) {
	out := WorkspaceProductPage{Items: []WorkspaceProduct{}}
	if limit < 1 || limit > 200 || offset < 0 {
		return out, ErrInvalidProductPage
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		w, err := WorkspaceTx(ctx, tx, account, team, false)
		if err != nil {
			return err
		}
		catalog := "personal_subscription"
		if w.TeamID != "" {
			catalog = "team_subscription"
		}
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM billing_products WHERE provider='stripe' AND active AND (catalog_type=$1 OR catalog_type='top_up')`, catalog).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT code,catalog_ref,verified_unit_amount,verified_currency,verified_interval,version,catalog_type FROM billing_products WHERE provider='stripe' AND active AND (catalog_type=$1 OR catalog_type='top_up') ORDER BY code LIMIT $2 OFFSET $3`, catalog, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := workspaceProduct(rows)
			if err != nil {
				return err
			}
			if p.CatalogType == "top_up" {
				p.MaxQuantity = &s.topupMaxQuantity
			}
			out.Items = append(out.Items, p)
		}
		return rows.Err()
	})
	return out, err
}

type workspaceOperation struct {
	WorkspaceID, Actor, Kind, Hash, State string
	Ciphertext, RequestCiphertext         []byte
	ProviderAttemptedAt                   sql.NullTime
	UpdatedAt                             time.Time
}

type checkoutProviderRequest struct {
	CustomerID, PriceID, PredecessorSubscriptionID string
	Quantity                                       int
	SuccessURL, CancelURL                          string
}

func workspaceRequestHash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (s *WorkspaceService) Checkout(ctx context.Context, account, email, team, productCode, key, successURL, cancelURL string, productVersion int64) (CheckoutSession, error) {
	if s.stripe == nil {
		return CheckoutSession{}, ErrStripeUnavailable
	}
	if s.key == "" {
		return CheckoutSession{}, ErrWorkspaceEncryptionUnavailable
	}
	requestHash, err := workspaceRequestHash(struct {
		Actor, Email, Team, Product, Success, Cancel string
		ProductVersion                               int64
	}{account, email, team, productCode, successURL, cancelURL, productVersion})
	if err != nil {
		return CheckoutSession{}, err
	}
	// The newest checkout's Stripe state is read before the transaction: WorkspaceTx holds the team write
	// lock, and no provider call runs under a database lock (F21-20). The transaction uses it only if that
	// checkout is still the newest.
	priorState, err := s.priorCheckoutState(ctx, account, team)
	if err != nil {
		return CheckoutSession{}, err
	}
	var workspace Workspace
	var priceID string
	var replay *CheckoutSession
	var providerRequest checkoutProviderRequest
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		workspace, err = WorkspaceTx(ctx, tx, account, team, true)
		if err != nil {
			return err
		}
		expected := "personal_subscription"
		if workspace.TeamID != "" {
			expected = "team_subscription"
		}
		var catalog string
		var currentVersion int64
		if err = tx.QueryRow(ctx, `SELECT provider_price_id,catalog_type,version FROM billing_products WHERE code=$1 AND provider='stripe' AND active`, productCode).Scan(&priceID, &catalog, &currentVersion); err != nil {
			if err == pgx.ErrNoRows {
				return ErrUnknownProduct
			}
			return err
		}
		if currentVersion != productVersion {
			return ErrProductChanged
		}
		if catalog != expected {
			return ErrUnknownProduct
		}
		quantity := 1
		if workspace.TeamID != "" {
			quantity = workspace.Members
		}
		providerRequest = checkoutProviderRequest{CustomerID: workspace.CustomerID, PriceID: priceID, PredecessorSubscriptionID: workspace.SubscriptionID, Quantity: quantity, SuccessURL: successURL, CancelURL: cancelURL}
		op, found, err := loadWorkspaceOperation(ctx, tx, key)
		if err != nil {
			return err
		}
		if found {
			if op.WorkspaceID != workspace.ID || op.Actor != account || op.Kind != "checkout" || op.Hash != requestHash {
				return ErrIdempotencyConflict
			}
			switch op.State {
			case "succeeded":
				var v CheckoutSession
				if err := s.decryptResult(op.Ciphertext, &v); err != nil {
					return err
				}
				replay = &v
				return nil
			case "uncertain":
				if op.ProviderAttemptedAt.Valid && time.Since(op.ProviderAttemptedAt.Time) >= 24*time.Hour {
					return ErrStripeOperationExpired
				}
				if err = s.decryptResult(op.RequestCiphertext, &providerRequest); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE billing_workspace_operations SET state='pending',updated_at=now() WHERE idempotency_key=$1 AND state='uncertain'`, key)
				return err
			case "failed":
				return ErrWorkspaceOperationFailed
			default:
				if time.Since(op.UpdatedAt) < time.Minute {
					return ErrCheckoutPending
				}
				if op.ProviderAttemptedAt.Valid && time.Since(op.ProviderAttemptedAt.Time) >= 24*time.Hour {
					return ErrStripeOperationExpired
				}
				if err = s.decryptResult(op.RequestCiphertext, &providerRequest); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE billing_workspace_operations SET updated_at=now() WHERE idempotency_key=$1`, key)
				return err
			}
		}
		if workspace.SubscriptionID != "" && workspace.State != "canceled" && workspace.State != "incomplete_expired" {
			return ErrWorkspaceSubscriptionExists
		}
		// An abandoned checkout can also belong to a replacement of a canceled
		// subscription. Only ignore receipts predating that subscription state.
		// Checkout operations are serialized per workspace. If the newest
		// successful receipt has another predecessor, an older receipt cannot be
		// the outstanding checkout for the current subscription.
		var priorKey string
		var priorCipher, requestCipher []byte
		e := tx.QueryRow(ctx, `SELECT idempotency_key,result_ciphertext,request_ciphertext FROM billing_workspace_operations WHERE workspace_id=$1 AND kind='checkout' AND state='succeeded' ORDER BY updated_at DESC LIMIT 1`, workspace.ID).Scan(&priorKey, &priorCipher, &requestCipher)
		if e == nil {
			var request checkoutProviderRequest
			if e = s.decryptResult(requestCipher, &request); e != nil {
				return e
			}
			if request.PredecessorSubscriptionID == workspace.SubscriptionID {
				var prior CheckoutSession
				if e = s.decryptResult(priorCipher, &prior); e != nil {
					return e
				}
				if priorState.key != priorKey {
					// Another checkout finished since the read before the lock; the caller retries.
					return ErrCheckoutPending
				}
				if priorState.err != nil {
					return priorState.err
				}
				state := priorState.session
				if state.ID != prior.ProviderSessionID || state.CustomerID != prior.ProviderCustomerID || state.CheckoutOperation != priorKey {
					return ErrProviderOutcomeUnknown
				}
				if state.Status != "expired" {
					return ErrCheckoutPending
				}
			}
		} else if e != pgx.ErrNoRows {
			return e
		}
		b, e := json.Marshal(providerRequest)
		if e != nil {
			return e
		}
		cipher, e := secrets.Encrypt(s.key, string(b))
		if e != nil {
			return e
		}
		_, err = tx.Exec(ctx, `INSERT INTO billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext) VALUES($1,$2,$3,'checkout',$4,'pending',$5)`, key, workspace.ID, account, requestHash, cipher)
		return err
	})
	if err != nil {
		return CheckoutSession{}, err
	}
	if replay != nil {
		state, e := s.stripe.GetCheckoutSession(ctx, replay.ProviderSessionID)
		if e != nil {
			return CheckoutSession{}, e
		}
		if state.ID != replay.ProviderSessionID || state.CustomerID != replay.ProviderCustomerID || state.CheckoutOperation != key {
			return CheckoutSession{}, ErrProviderOutcomeUnknown
		}
		switch state.Status {
		case "open":
			if err := s.reauthorizeBilling(ctx, account, team); err != nil {
				return CheckoutSession{}, err
			}
			if state.URL != "" {
				replay.URL = state.URL
			}
			return *replay, nil
		case "expired":
			return CheckoutSession{}, ErrCheckoutSessionExpired
		default:
			return CheckoutSession{}, ErrCheckoutPending
		}
	}

	if providerRequest.CustomerID == "" {
		if _, err = s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET provider_attempted_at=COALESCE(provider_attempted_at,now()) WHERE idempotency_key=$1 AND state='pending'`, key); err != nil {
			return CheckoutSession{}, err
		}
		customerID, callErr := s.stripe.CreateCustomer(ctx, workspace.ID, email, key+":customer")
		if callErr != nil {
			return CheckoutSession{}, s.failOperation(ctx, key, callErr)
		}
		err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			w, e := WorkspaceTx(ctx, tx, account, team, true)
			if e != nil {
				return e
			}
			if w.CustomerID != "" && w.CustomerID != customerID {
				return errors.New("workspace already has a different provider customer")
			}
			_, e = tx.Exec(ctx, `UPDATE billing_workspaces SET provider_customer_id=$2,updated_at=now(),revision=revision+1 WHERE id=$1`, w.ID, customerID)
			return e
		})
		if err != nil {
			_ = s.markOperation(ctx, key, "uncertain")
			return CheckoutSession{}, errors.Join(ErrProviderOutcomeUnknown, err)
		}
		workspace.CustomerID = customerID
		providerRequest.CustomerID = customerID
		b, _ := json.Marshal(providerRequest)
		cipher, e := secrets.Encrypt(s.key, string(b))
		if e != nil {
			return CheckoutSession{}, e
		}
		if _, e = s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET request_ciphertext=$2,updated_at=now() WHERE idempotency_key=$1 AND state='pending'`, key, cipher); e != nil {
			return CheckoutSession{}, e
		}
	}
	if providerRequest.Quantity < 1 {
		_ = s.markOperation(ctx, key, "failed")
		return CheckoutSession{}, errors.New("workspace has no billable members")
	}
	_, err = s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET provider_attempted_at=COALESCE(provider_attempted_at,now()) WHERE idempotency_key=$1 AND state='pending'`, key)
	if err != nil {
		return CheckoutSession{}, err
	}
	session, err := s.stripe.CreateSubscriptionCheckout(ctx, StripeCheckoutInput{CustomerID: providerRequest.CustomerID, PriceID: providerRequest.PriceID, Quantity: providerRequest.Quantity, SuccessURL: providerRequest.SuccessURL, CancelURL: providerRequest.CancelURL, IdempotencyKey: key})
	if err != nil {
		return CheckoutSession{}, s.failOperation(ctx, key, err)
	}
	if err = s.completeOperation(ctx, key, session); err != nil {
		_ = s.markOperation(ctx, key, "uncertain")
		return CheckoutSession{}, errors.Join(ErrProviderOutcomeUnknown, err)
	}
	if err := s.reauthorizeBilling(ctx, account, team); err != nil {
		return CheckoutSession{}, err
	}
	return session, nil
}

func (s *WorkspaceService) reauthorizeBilling(ctx context.Context, account, team string) error {
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := WorkspaceTx(ctx, tx, account, team, true)
		return err
	})
}

func loadWorkspaceOperation(ctx context.Context, tx *db.Tx, key string) (workspaceOperation, bool, error) {
	var o workspaceOperation
	err := tx.QueryRow(ctx, `SELECT workspace_id,actor_user_id,kind,request_hash,state,result_ciphertext,request_ciphertext,provider_attempted_at,updated_at FROM billing_workspace_operations WHERE idempotency_key=$1`, key).Scan(&o.WorkspaceID, &o.Actor, &o.Kind, &o.Hash, &o.State, &o.Ciphertext, &o.RequestCiphertext, &o.ProviderAttemptedAt, &o.UpdatedAt)
	if err == pgx.ErrNoRows {
		return o, false, nil
	}
	return o, err == nil, err
}

func (s *WorkspaceService) decryptResult(cipher []byte, out any) error {
	plain, err := secrets.Decrypt(s.key, cipher)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(plain), out)
}
func (s *WorkspaceService) encryptResult(value any) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return secrets.Encrypt(s.key, string(b))
}
func (s *WorkspaceService) completeOperation(ctx context.Context, key string, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	cipher, err := secrets.Encrypt(s.key, string(b))
	if err != nil {
		return err
	}
	tag, err := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET state='succeeded',result_ciphertext=$2,updated_at=now() WHERE idempotency_key=$1 AND state='pending'`, key, cipher)
	if err != nil {
		return err
	}
	n, _ := tag.RowsAffected()
	if n != 1 {
		return errors.New("billing operation completion lost")
	}
	return nil
}
func (s *WorkspaceService) markOperation(ctx context.Context, key, state string) error {
	_, err := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET state=$2,updated_at=now() WHERE idempotency_key=$1 AND state='pending'`, key, state)
	return err
}
func stripeOutcomeUncertain(err error) bool {
	var api StripeAPIError
	return errors.Is(err, ErrProviderOutcomeUnknown) || !errors.As(err, &api) || api.StatusCode == http.StatusConflict || api.StatusCode == http.StatusTooManyRequests || api.StatusCode >= http.StatusInternalServerError
}
func (s *WorkspaceService) failOperation(ctx context.Context, key string, providerErr error) error {
	if stripeOutcomeUncertain(providerErr) {
		_ = s.markOperation(ctx, key, "uncertain")
		return errors.Join(ErrProviderOutcomeUnknown, providerErr)
	}
	_ = s.markOperation(ctx, key, "failed")
	return providerErr
}

func (s *WorkspaceService) HandleWebhook(ctx context.Context, event StripeWebhook) (bool, error) {
	if s.stripe == nil {
		return false, ErrStripeUnavailable
	}
	if event.ID == "" {
		return false, nil
	}
	var already bool
	if err := s.store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.stripe_events WHERE id=$1)`, event.ID).Scan(&already); err != nil {
		return false, err
	}
	if already {
		return false, nil
	}
	if handled, processed, err := s.handleTeamCreationWebhook(ctx, event); handled || err != nil {
		return processed, err
	}
	if handled, processed, err := s.handleTopupWebhook(ctx, event); handled {
		return processed, err
	}
	subscriptionID := event.SubscriptionID()
	if subscriptionID == "" {
		return false, nil
	}
	if event.Type == TrialWillEndEvent {
		// Before reconciling: reconciliation records the event, and a redelivery
		// after a failed notice must still queue it (the notice is idempotent).
		if err := s.noticeTeamTrialEnding(ctx, subscriptionID); err != nil {
			return false, err
		}
	}
	return s.reconcileSubscription(ctx, subscriptionID, &event)
}

// reconcileSubscription applies an authoritative Stripe snapshot. Webhooks and
// operator recovery share this path so neither can manufacture subscription state.
func (s *WorkspaceService) reconcileSubscription(ctx context.Context, subscriptionID string, event *StripeWebhook) (bool, error) {
	// One reconcile per subscription at a time in this process: the Stripe read happens before any
	// database lock (F21-20), and the snapshot applied is the one read after the previous reconcile.
	gate := &subscriptionReconcileGates[subscriptionGate(subscriptionID)]
	gate.Lock()
	defer gate.Unlock()
	initial, err := s.stripe.GetSubscription(ctx, subscriptionID)
	if err != nil {
		return false, err
	}
	processed := false
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var workspaceID string
		if err := tx.QueryRow(ctx, `SELECT id FROM billing_workspaces WHERE provider_customer_id=$1 OR provider_subscription_id=$2 FOR UPDATE`, initial.CustomerID, subscriptionID).Scan(&workspaceID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, workspaceID); err != nil {
			return err
		}
		var exists bool
		if event != nil {
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM stripe_events WHERE id=$1)`, event.ID).Scan(&exists); err != nil || exists {
				return err
			}
		}
		current := initial
		if current.CustomerID != initial.CustomerID || current.ID != subscriptionID {
			return errors.New("stripe subscription identity changed during reconciliation")
		}
		// Stripe can serve the subscription from before a change it just
		// announced. Do not acknowledge that event: the provider retries it and the
		// next read reflects the cancellation.
		if event != nil {
			if cancelling, known := event.ReportsCancellation(); known && cancelling != current.CancelAtPeriodEnd &&
				time.Since(time.Unix(event.Created, 0)) < staleProviderReadWindow {
				return ErrProviderOutcomeUnknown
			}
		}
		var team sql.NullString
		var existingSubscription sql.NullString
		var existingState string
		if err = tx.QueryRow(ctx, `SELECT team_id,provider_subscription_id,state FROM billing_workspaces WHERE id=$1`, workspaceID).Scan(&team, &existingSubscription, &existingState); err != nil {
			return err
		}
		newSubscription := !existingSubscription.Valid || existingSubscription.String != current.ID
		if newSubscription {
			allowed := current.CheckoutOperation != ""
			if existingSubscription.Valid {
				allowed = allowed && (existingState == "canceled" || existingState == "incomplete_expired")
			}
			if allowed {
				op, found, loadErr := loadWorkspaceOperation(ctx, tx, current.CheckoutOperation)
				if loadErr != nil {
					return loadErr
				}
				// Stripe can publish the subscription before the checkout caller has
				// durably recorded its successful result. Do not acknowledge and
				// deduplicate that event: a provider retry must be able to reconcile it
				// after checkout recovery completes the matching operation.
				if found && op.WorkspaceID == workspaceID && op.Kind == "checkout" &&
					(op.State == "pending" || op.State == "uncertain") {
					return ErrProviderOutcomeUnknown
				}
				allowed = found && op.WorkspaceID == workspaceID && op.Kind == "checkout" && op.State == "succeeded"
				if allowed {
					var request checkoutProviderRequest
					if err = s.decryptResult(op.RequestCiphertext, &request); err != nil {
						return err
					}
					predecessor := ""
					if existingSubscription.Valid {
						predecessor = existingSubscription.String
					}
					allowed = request.PredecessorSubscriptionID == predecessor
				}
			}
			if !allowed {
				if event != nil {
					if _, err = tx.Exec(ctx, `INSERT INTO stripe_events(id,event_type,subscription_id) VALUES($1,$2,$3)`, event.ID, event.Type, current.ID); err != nil {
						return err
					}
				}
				processed = true
				return nil
			}
		}
		var productCode, catalog string
		if err = tx.QueryRow(ctx, `SELECT p.code,p.catalog_type FROM billing_workspaces w JOIN billing_products p ON p.provider='stripe' AND p.provider_price_id=$2 WHERE w.id=$1`, workspaceID, current.PriceID).Scan(&productCode, &catalog); err != nil {
			return err
		}
		expected := "personal_subscription"
		if team.Valid {
			expected = "team_subscription"
		}
		if catalog != expected {
			return errors.New("stripe product does not match workspace scope")
		}
		if err = updateRenewalGraceTx(ctx, tx, workspaceID, current, event); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE billing_workspaces SET provider_subscription_id=$2,provider_item_id=$3,product_code=$4,state=$5,quantity=$6,period_start=$7,period_end=$8,cancel_at_period_end=$9,bandwidth_anchor=CASE WHEN $5 IN ('active','trialing') THEN coalesce(bandwidth_anchor,$7) ELSE bandwidth_anchor END,updated_at=now(),revision=revision+1 WHERE id=$1 AND provider_customer_id=$10`, workspaceID, current.ID, current.ItemID, productCode, current.Status, current.Quantity, current.PeriodStart, current.PeriodEnd, current.CancelAtPeriodEnd, current.CustomerID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE billing_workspaces SET scheduled_quantity=NULL,scheduled_quantity_at=NULL WHERE id=$1 AND scheduled_quantity_at<=$2 AND quantity=scheduled_quantity`, workspaceID, current.PeriodStart); err != nil {
			return err
		}
		// A cancelling subscription never renews, so no scheduled seat reduction
		// can take effect; Stripe releases the schedule when the cancel is set.
		if current.CancelAtPeriodEnd {
			if _, err = tx.Exec(ctx, `UPDATE billing_workspaces SET scheduled_quantity=NULL,scheduled_quantity_at=NULL WHERE id=$1`, workspaceID); err != nil {
				return err
			}
		}
		if event != nil {
			if _, err = tx.Exec(ctx, `INSERT INTO stripe_events(id,event_type,subscription_id) VALUES($1,$2,$3)`, event.ID, event.Type, current.ID); err != nil {
				return err
			}
		}
		processed = true
		return nil
	})
	return processed, err
}

// checkoutProviderState is the provider's view of a workspace's newest successful checkout, read with no
// database lock held.
type checkoutProviderState struct {
	key     string
	session StripeCheckoutState
	err     error
}

func (s *WorkspaceService) priorCheckoutState(ctx context.Context, account, team string) (checkoutProviderState, error) {
	workspaceID := "personal:" + account
	if team != "" {
		workspaceID = "team:" + team
	}
	var out checkoutProviderState
	var priorCipher []byte
	err := s.store.SQL().QueryRowContext(ctx, `SELECT idempotency_key,result_ciphertext FROM paperboat.billing_workspace_operations WHERE workspace_id=$1 AND kind='checkout' AND state='succeeded' ORDER BY updated_at DESC LIMIT 1`, workspaceID).Scan(&out.key, &priorCipher)
	if errors.Is(err, sql.ErrNoRows) {
		return checkoutProviderState{}, nil
	}
	if err != nil {
		return checkoutProviderState{}, err
	}
	var prior CheckoutSession
	if err = s.decryptResult(priorCipher, &prior); err != nil {
		return checkoutProviderState{}, err
	}
	out.session, out.err = s.stripe.GetCheckoutSession(ctx, prior.ProviderSessionID)
	return out, nil
}

// subscriptionReconcileGates serialize reconciles of one subscription in this process (bounded: a
// subscription hashes onto one of them).
var subscriptionReconcileGates [64]sync.Mutex

func subscriptionGate(subscriptionID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(subscriptionID))
	return int(h.Sum32() % uint32(len(subscriptionReconcileGates)))
}
