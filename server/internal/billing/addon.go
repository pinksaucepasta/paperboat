package billing

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A team's SSO and SCIM add-ons are a second item on its Stripe subscription. Their prices are billing_products of catalog
// type AddonCatalog with the code AddonProductCode; PutProduct accepts one only at the price in config.Business.
const AddonCatalog = "team_addon"

// Addons are the add-ons a team can have; the empty string is none.
var Addons = []string{"sso", "scim", "sso_scim"}

var (
	ErrAddonInvalid      = errors.New("unknown add-on")
	ErrAddonConsent      = errors.New("the refund policy must be accepted to add or change an add-on")
	ErrAddonUnavailable  = errors.New("this add-on has no price configured")
	ErrAddonSubscription = errors.New("an active team subscription with no pending cancellation or seat reduction is required to change the add-on")
)

func AddonProductCode(addon, interval string) string { return "addon-" + addon + "-" + interval }

// addonOfCode is the add-on a product code sells, "" when it is not an add-on code.
func addonOfCode(code string) string {
	for _, a := range Addons {
		if strings.HasPrefix(code, "addon-"+a+"-") && (strings.HasSuffix(code, "-month") || strings.HasSuffix(code, "-year")) {
			return a
		}
	}
	return ""
}

func (s *OperatorService) addonPriceMatches(code string, p StripePrice) bool {
	for _, a := range Addons {
		if code == AddonProductCode(a, p.Interval) {
			cents, ok := s.business.AddonPriceCents(a, p.Interval)
			return ok && p.Recurring && p.UnitAmount == cents && p.Currency == "usd"
		}
	}
	return false
}

// AddonProvider changes the add-on item of a subscription.
type AddonProvider interface {
	ChangeAddon(context.Context, AddonChange, string) (StripeSubscription, error)
}

// AddonChange adds (AddonItemID empty), switches or, with an empty PriceID, removes the add-on item.
type AddonChange struct {
	SubscriptionID, CustomerID, PlanItemID, AddonItemID, PriceID, Addon string
}

func (c HTTPStripeClient) ChangeAddon(ctx context.Context, in AddonChange, key string) (StripeSubscription, error) {
	v := url.Values{"expand[]": {"latest_invoice"}} // the response parser reads the invoice as an object
	if in.PriceID == "" {
		// Removal gives no credit (refund policy): the add-on keeps working to the end of the paid period (addon_until).
		v.Set("items[0][id]", in.AddonItemID)
		v.Set("items[0][deleted]", "true")
		v.Set("proration_behavior", "none")
	} else {
		if in.AddonItemID != "" {
			v.Set("items[0][id]", in.AddonItemID)
		} else {
			v.Set("items[0][quantity]", "1")
		}
		v.Set("items[0][price]", in.PriceID)
		v.Set("items[0][metadata][paperboat_addon]", in.Addon)
		v.Set("proration_behavior", "always_invoice")
		v.Set("payment_behavior", "error_if_incomplete")
	}
	var out stripeSubscriptionResponse
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(in.SubscriptionID), key, v, &out); err != nil {
		return StripeSubscription{}, err
	}
	s, err := out.subscription()
	if err != nil {
		return StripeSubscription{}, err
	}
	if s.ID != in.SubscriptionID || s.CustomerID != in.CustomerID || s.ItemID != in.PlanItemID || s.AddonPriceID != in.PriceID {
		return StripeSubscription{}, errors.New("stripe subscription update response does not match request")
	}
	return s, nil
}

type addonState struct {
	addon, itemID string // itemID is empty once the item was removed and the add-on only runs out its paid period
}

func (a addonState) running() string {
	if a.itemID == "" {
		return ""
	}
	return a.addon
}

// ChangeAddon sets a team's add-on ("" removes it) on its subscription and returns the new overview. Adding or switching
// charges the prorated difference now, so the caller must have accepted the refund policy.
func (s *WorkspaceService) ChangeAddon(ctx context.Context, account, team, addon string, consent bool, key string) (Workspace, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	provider, ok := s.stripe.(AddonProvider)
	if !ok {
		return Workspace{}, ErrStripeUnavailable
	}
	if addon != "" && addonOfCode(AddonProductCode(addon, "month")) != addon {
		return Workspace{}, ErrAddonInvalid
	}
	if addon != "" && !consent {
		return Workspace{}, ErrAddonConsent
	}
	if key == "" || len(key) > 200 {
		return Workspace{}, ErrIdempotencyConflict
	}
	var in AddonChange
	var previous string
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		w, err := WorkspaceTx(ctx, tx, account, team, true)
		if err != nil {
			return err
		}
		if team == "" || w.SubscriptionID == "" || w.ItemID == "" || (w.State != "active" && w.State != "trialing") || w.CancelAtPeriodEnd || w.ScheduledQuantity != nil || w.PeriodEnd == nil || !w.PeriodEnd.After(time.Now()) {
			return ErrAddonSubscription
		}
		var cur addonState
		if err = tx.QueryRow(ctx, `SELECT coalesce(addon,''),coalesce(addon_item_id,'') FROM billing_workspaces WHERE id=$1`, w.ID).Scan(&cur.addon, &cur.itemID); err != nil {
			return err
		}
		previous = cur.running()
		in = AddonChange{SubscriptionID: w.SubscriptionID, CustomerID: w.CustomerID, PlanItemID: w.ItemID, AddonItemID: cur.itemID, Addon: addon}
		if addon == "" || addon == previous {
			return nil
		}
		var interval string
		if err = tx.QueryRow(ctx, `SELECT verified_interval FROM billing_products WHERE code=$1 AND provider='stripe' AND verified_interval IS NOT NULL`, w.ProductCode).Scan(&interval); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT provider_price_id FROM billing_products WHERE code=$1 AND provider='stripe' AND catalog_type=$2 AND active AND verified_unit_amount IS NOT NULL`, AddonProductCode(addon, interval), AddonCatalog).Scan(&in.PriceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAddonUnavailable
		}
		return err
	})
	if err != nil {
		return Workspace{}, err
	}
	if addon != previous {
		if addon == "" && in.AddonItemID == "" {
			return s.Overview(ctx, account, team)
		}
		sub, err := provider.ChangeAddon(ctx, in, key)
		if err != nil {
			return Workspace{}, err
		}
		if _, err = s.reconcileSubscriptionWith(ctx, sub, nil); err != nil {
			return Workspace{}, err
		}
		err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			return audit.NewWriter(nil).WriteTx(ctx, tx, audit.Event{ActorUserID: account, ActorType: audit.ActorUser, EventType: "billing.addon_changed", ResourceType: "team", ResourceID: team,
				Metadata: map[string]any{"addon": addon, "previous_addon": previous, "accepted_refund_policy": consent}})
		})
		if err != nil {
			return Workspace{}, err
		}
	}
	return s.Overview(ctx, account, team)
}

// syncAddonTx records the add-on item of the subscription snapshot on the workspace. An item whose price is not an add-on
// product counts as removed. A removed item leaves the add-on running to the end of the paid period.
func syncAddonTx(ctx context.Context, tx *db.Tx, workspaceID string, current StripeSubscription, newSubscription bool) error {
	if newSubscription {
		if _, err := tx.Exec(ctx, `UPDATE billing_workspaces SET addon=NULL,addon_item_id=NULL,addon_price_id=NULL,addon_until=NULL WHERE id=$1`, workspaceID); err != nil {
			return err
		}
	}
	var addon string
	if current.AddonItemID != "" {
		var code string
		err := tx.QueryRow(ctx, `SELECT code FROM billing_products WHERE provider='stripe' AND provider_price_id=$1 AND catalog_type=$2`, current.AddonPriceID, AddonCatalog).Scan(&code)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		addon = addonOfCode(code)
	}
	if addon != "" {
		_, err := tx.Exec(ctx, `UPDATE billing_workspaces SET addon=$2,addon_item_id=$3,addon_price_id=$4,addon_until=NULL WHERE id=$1`, workspaceID, addon, current.AddonItemID, current.AddonPriceID)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE billing_workspaces SET addon_item_id=NULL,addon_price_id=NULL,addon_until=coalesce(addon_until,$2) WHERE id=$1 AND addon IS NOT NULL`, workspaceID, current.PeriodEnd)
	return err
}
