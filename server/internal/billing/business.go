package billing

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// Business billing (PLAN F19-11/12): invoices, billing details and pay by invoice. Stripe owns the
// data; these methods authorize (billing.manage), validate and forward.

var (
	ErrNoBillingCustomer      = errors.New("this workspace has no billing account yet; buy a plan first")
	ErrInvalidBillingDetails  = errors.New("check the billing email, company name, address and tax ID")
	ErrPayByInvoiceIneligible = errors.New("pay by invoice needs an active yearly Team plan with enough seats")
)

// ConfigureBusiness sets the business billing limits (config.Business).
func (s *WorkspaceService) ConfigureBusiness(cfg config.Business) { s.business = cfg }

func (s *WorkspaceService) businessClient() (StripeBusinessClient, error) {
	client, ok := s.stripe.(StripeBusinessClient)
	if !ok {
		return nil, ErrStripeUnavailable
	}
	return client, nil
}

// managedWorkspace authorizes billing.manage in a short transaction; Stripe is called after it.
func (s *WorkspaceService) managedWorkspace(ctx context.Context, account, team string) (Workspace, error) {
	var out Workspace
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		out, err = WorkspaceTx(ctx, tx, account, team, true)
		return err
	})
	return out, err
}

type InvoicePage struct {
	Items      []Invoice `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

const invoicePageSize = 24

// Invoices lists the workspace's invoices newest first; cursor is the last invoice ID of the previous page.
func (s *WorkspaceService) Invoices(ctx context.Context, account, team, cursor string) (InvoicePage, error) {
	out := InvoicePage{Items: []Invoice{}}
	client, err := s.businessClient()
	if err != nil {
		return out, err
	}
	if len(cursor) > 255 {
		return out, ErrInvalidBillingDetails
	}
	w, err := s.managedWorkspace(ctx, account, team)
	if err != nil || w.CustomerID == "" {
		return out, err
	}
	items, more, err := client.ListInvoices(ctx, w.CustomerID, cursor, invoicePageSize)
	if err != nil {
		return out, err
	}
	out.Items = items
	if more && len(items) > 0 {
		out.NextCursor = items[len(items)-1].ID
	}
	return out, nil
}

func (s *WorkspaceService) BillingDetails(ctx context.Context, account, team string) (BillingDetails, error) {
	client, err := s.businessClient()
	if err != nil {
		return BillingDetails{}, err
	}
	w, err := s.managedWorkspace(ctx, account, team)
	if err != nil {
		return BillingDetails{}, err
	}
	if w.CustomerID == "" {
		return BillingDetails{}, ErrNoBillingCustomer
	}
	return client.GetBillingDetails(ctx, w.CustomerID)
}

var (
	countryPattern   = regexp.MustCompile(`^[A-Z]{2}$`)
	taxTypePattern   = regexp.MustCompile(`^[a-z]{2}_[a-z]{2,8}$`)
	taxValuePattern  = regexp.MustCompile(`^[A-Za-z0-9 .\-/]{2,40}$`)
	billingEmailRule = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

func boundedText(value string, max int) bool {
	return utf8.ValidString(value) && len(value) <= max && !strings.ContainsAny(value, "\r\n\x00")
}

func (d BillingDetails) validate() bool {
	a := d.Address
	ok := len(d.Email) <= 254 && billingEmailRule.MatchString(d.Email) && boundedText(d.Name, 200)
	for _, field := range []string{a.Line1, a.Line2, a.City, a.State, a.PostalCode} {
		ok = ok && boundedText(field, 200)
	}
	hasAddress := a.Line1 != "" || a.Line2 != "" || a.City != "" || a.State != "" || a.PostalCode != ""
	ok = ok && (a.Country == "" && !hasAddress || countryPattern.MatchString(a.Country))
	if d.TaxID != nil {
		ok = ok && taxTypePattern.MatchString(d.TaxID.Type) && taxValuePattern.MatchString(d.TaxID.Value)
	}
	return ok
}

// UpdateBillingDetails replaces the billing email, company name, address and tax ID on the Stripe
// customer; Stripe prints them on every later invoice and validates the tax ID.
func (s *WorkspaceService) UpdateBillingDetails(ctx context.Context, account, team string, in BillingDetails, key string) (BillingDetails, error) {
	client, err := s.businessClient()
	if err != nil {
		return BillingDetails{}, err
	}
	in.Email = strings.TrimSpace(in.Email)
	in.Name = strings.TrimSpace(in.Name)
	if in.TaxID != nil {
		in.TaxID.Verification = ""
	}
	if !in.validate() || key == "" || len(key) > 200 {
		return BillingDetails{}, ErrInvalidBillingDetails
	}
	w, err := s.managedWorkspace(ctx, account, team)
	if err != nil {
		return BillingDetails{}, err
	}
	if w.CustomerID == "" {
		return BillingDetails{}, ErrNoBillingCustomer
	}
	if err = client.UpdateBillingDetails(ctx, w.CustomerID, in, key+".customer"); err != nil {
		return BillingDetails{}, err
	}
	if err = client.SetTaxID(ctx, w.CustomerID, in.TaxID, key+".tax_id"); err != nil {
		return BillingDetails{}, err
	}
	if err = s.recordBilling(ctx, account, w, "billing.details_updated", map[string]any{"tax_id": in.TaxID != nil}); err != nil {
		return BillingDetails{}, err
	}
	return client.GetBillingDetails(ctx, w.CustomerID)
}

// CollectionState is how the workspace pays and whether pay by invoice may be turned on.
type CollectionState struct {
	Collection
	Eligible       bool `json:"eligible"`
	MinSeats       int  `json:"min_seats"`
	DefaultDueDays int  `json:"default_days_until_due"`
}

func (s *WorkspaceService) collectionState(ctx context.Context, w Workspace, current Collection) (CollectionState, error) {
	out := CollectionState{Collection: current, MinSeats: s.business.InvoiceMinSeats, DefaultDueDays: s.business.InvoiceDaysUntilDue}
	if w.TeamID == "" || w.SubscriptionID == "" || w.State != "active" || w.CancelAtPeriodEnd || w.Quantity < s.business.InvoiceMinSeats {
		return out, nil
	}
	err := s.store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM billing_products WHERE code=$1 AND verified_interval='year')`, w.ProductCode).Scan(&out.Eligible)
	return out, err
}

func (s *WorkspaceService) PayByInvoice(ctx context.Context, account, team string) (CollectionState, error) {
	client, err := s.businessClient()
	if err != nil {
		return CollectionState{}, err
	}
	w, err := s.managedWorkspace(ctx, account, team)
	if err != nil {
		return CollectionState{}, err
	}
	current := Collection{}
	if w.SubscriptionID != "" {
		if current, err = client.GetCollection(ctx, w.SubscriptionID); err != nil {
			return CollectionState{}, err
		}
	}
	return s.collectionState(ctx, w, current)
}

// SetPayByInvoice switches an eligible Team subscription to pay by invoice (Stripe send_invoice,
// net business.invoice_days_until_due) or back to card payment. Turning it off is always allowed.
func (s *WorkspaceService) SetPayByInvoice(ctx context.Context, account, team string, on bool, key string) (CollectionState, error) {
	client, err := s.businessClient()
	if err != nil {
		return CollectionState{}, err
	}
	if key == "" || len(key) > 200 {
		return CollectionState{}, ErrInvalidBillingDetails
	}
	w, err := s.managedWorkspace(ctx, account, team)
	if err != nil {
		return CollectionState{}, err
	}
	if w.SubscriptionID == "" {
		return CollectionState{}, ErrPayByInvoiceIneligible
	}
	state, err := s.collectionState(ctx, w, Collection{})
	if err != nil {
		return state, err
	}
	if on && !state.Eligible {
		return state, ErrPayByInvoiceIneligible
	}
	want := Collection{PayByInvoice: on}
	if on {
		want.DaysUntilDue = s.business.InvoiceDaysUntilDue
	}
	if state.Collection, err = client.SetCollection(ctx, w.SubscriptionID, want, key); err != nil {
		return state, err
	}
	return state, s.recordBilling(ctx, account, w, "billing.pay_by_invoice_changed", map[string]any{"active": on})
}

func (s *WorkspaceService) recordBilling(ctx context.Context, account string, w Workspace, action string, metadata map[string]any) error {
	resourceType, resourceID := "user", account
	if w.TeamID != "" {
		resourceType, resourceID = "team", w.TeamID
	}
	return audit.NewWriter(s.store).Write(ctx, audit.Event{ActorUserID: account, ActorType: audit.ActorUser, EventType: action, ResourceType: resourceType, ResourceID: resourceID, Metadata: metadata})
}
