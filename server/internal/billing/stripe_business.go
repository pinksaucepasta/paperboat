package billing

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// StripeBusinessClient is the Stripe API used by business billing (PLAN F19-11/12). Stripe is the
// only owner of invoices and billing details; nothing here is copied into our database.
type StripeBusinessClient interface {
	ListInvoices(ctx context.Context, customerID, startingAfter string, limit int) ([]Invoice, bool, error)
	GetBillingDetails(ctx context.Context, customerID string) (BillingDetails, error)
	UpdateBillingDetails(ctx context.Context, customerID string, in BillingDetails, key string) error
	SetTaxID(ctx context.Context, customerID string, taxID *TaxID, key string) error
	GetCollection(ctx context.Context, subscriptionID string) (Collection, error)
	SetCollection(ctx context.Context, subscriptionID string, in Collection, key string) (Collection, error)
}

// businessCheckout lets a company enter its tax ID, name and billing address at Checkout (stored
// on the Stripe customer, printed on invoices) and, with Stripe Tax on, has Stripe compute tax.
func (c HTTPStripeClient) businessCheckout(v url.Values) {
	v.Set("tax_id_collection[enabled]", "true")
	v.Set("customer_update[name]", "auto")
	v.Set("customer_update[address]", "auto")
	if c.AutomaticTax {
		v.Set("automatic_tax[enabled]", "true")
		v.Set("billing_address_collection", "required")
	}
}

// Invoice is one Stripe invoice as the console shows it. Amounts are in the currency's minor unit.
type Invoice struct {
	ID          string     `json:"id"`
	Number      string     `json:"number"`
	Status      string     `json:"status"`
	Currency    string     `json:"currency"`
	Total       int64      `json:"total"`
	Tax         int64      `json:"tax"`
	AmountDue   int64      `json:"amount_due"`
	CreatedAt   time.Time  `json:"created_at"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	DueDate     *time.Time `json:"due_date,omitempty"`
	HostedURL   string     `json:"hosted_url,omitempty"`
	PDFURL      string     `json:"pdf_url,omitempty"`
}

type Address struct {
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

// TaxID is one customer tax ID; Type is a Stripe tax ID type such as eu_vat, gb_vat or us_ein.
type TaxID struct {
	Type         string `json:"type"`
	Value        string `json:"value"`
	Verification string `json:"verification,omitempty"`
}

// BillingDetails are printed on every invoice; Email receives invoices and receipts.
type BillingDetails struct {
	Email   string  `json:"email"`
	Name    string  `json:"name"`
	Address Address `json:"address"`
	TaxID   *TaxID  `json:"tax_id"`
}

// Collection says how a subscription is paid: automatically by card, or by invoice with a due date.
type Collection struct {
	PayByInvoice bool `json:"pay_by_invoice"`
	DaysUntilDue int  `json:"days_until_due,omitempty"`
}

type stripeInvoice struct {
	ID, Number, Status, Currency string
	Total                        int64
	AmountDue                    int64 `json:"amount_due"`
	TotalTaxes                   []struct {
		Amount int64 `json:"amount"`
	} `json:"total_taxes"`
	Created          int64  `json:"created"`
	PeriodStart      int64  `json:"period_start"`
	PeriodEnd        int64  `json:"period_end"`
	DueDate          int64  `json:"due_date"`
	HostedInvoiceURL string `json:"hosted_invoice_url"`
	InvoicePDF       string `json:"invoice_pdf"`
}

func (i stripeInvoice) invoice() Invoice {
	out := Invoice{ID: i.ID, Number: i.Number, Status: i.Status, Currency: i.Currency, Total: i.Total, AmountDue: i.AmountDue,
		CreatedAt: unixTime(i.Created), PeriodStart: unixTime(i.PeriodStart), PeriodEnd: unixTime(i.PeriodEnd),
		HostedURL: i.HostedInvoiceURL, PDFURL: i.InvoicePDF}
	for _, t := range i.TotalTaxes {
		out.Tax += t.Amount
	}
	if i.DueDate > 0 {
		due := unixTime(i.DueDate)
		out.DueDate = &due
	}
	return out
}

func unixTime(s int64) time.Time { return time.Unix(s, 0).UTC() }

// ListInvoices returns finalized invoices newest first (drafts are not shown to customers).
func (c HTTPStripeClient) ListInvoices(ctx context.Context, customerID, startingAfter string, limit int) ([]Invoice, bool, error) {
	q := url.Values{"customer": {customerID}, "limit": {strconv.Itoa(limit)}}
	if startingAfter != "" {
		q.Set("starting_after", startingAfter)
	}
	var out struct {
		Data    []stripeInvoice `json:"data"`
		HasMore bool            `json:"has_more"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/invoices?"+q.Encode(), "", nil, &out); err != nil {
		return nil, false, err
	}
	invoices := make([]Invoice, 0, len(out.Data))
	for _, i := range out.Data {
		if i.ID == "" {
			return nil, false, errors.New("invalid stripe invoice list response")
		}
		if i.Status != "draft" {
			invoices = append(invoices, i.invoice())
		}
	}
	return invoices, out.HasMore, nil
}

type stripeCustomer struct {
	ID, Email, Name string
	Address         *Address `json:"address"`
	TaxIDs          struct {
		Data []struct {
			ID, Type, Value string
			Verification    *struct {
				Status string `json:"status"`
			} `json:"verification"`
		} `json:"data"`
	} `json:"tax_ids"`
}

func (c HTTPStripeClient) customer(ctx context.Context, customerID string) (stripeCustomer, error) {
	var out stripeCustomer
	if err := c.do(ctx, http.MethodGet, "/v1/customers/"+url.PathEscape(customerID)+"?expand%5B%5D=tax_ids", "", nil, &out); err != nil {
		return out, err
	}
	if out.ID != customerID {
		return out, errors.New("invalid stripe customer response")
	}
	return out, nil
}

func (c HTTPStripeClient) GetBillingDetails(ctx context.Context, customerID string) (BillingDetails, error) {
	cu, err := c.customer(ctx, customerID)
	if err != nil {
		return BillingDetails{}, err
	}
	out := BillingDetails{Email: cu.Email, Name: cu.Name}
	if cu.Address != nil {
		out.Address = *cu.Address
	}
	if len(cu.TaxIDs.Data) > 0 {
		t := cu.TaxIDs.Data[0]
		out.TaxID = &TaxID{Type: t.Type, Value: t.Value}
		if t.Verification != nil {
			out.TaxID.Verification = t.Verification.Status
		}
	}
	return out, nil
}

func (c HTTPStripeClient) UpdateBillingDetails(ctx context.Context, customerID string, in BillingDetails, key string) error {
	v := url.Values{"email": {in.Email}, "name": {in.Name},
		"address[line1]": {in.Address.Line1}, "address[line2]": {in.Address.Line2}, "address[city]": {in.Address.City},
		"address[state]": {in.Address.State}, "address[postal_code]": {in.Address.PostalCode}, "address[country]": {in.Address.Country},
	}
	var out struct{ ID string }
	if err := c.do(ctx, http.MethodPost, "/v1/customers/"+url.PathEscape(customerID), key, v, &out); err != nil {
		return err
	}
	if out.ID != customerID {
		return errors.New("invalid stripe customer update response")
	}
	return nil
}

// SetTaxID makes taxID the customer's only tax ID (nil removes it). Stripe validates the format.
func (c HTTPStripeClient) SetTaxID(ctx context.Context, customerID string, taxID *TaxID, key string) error {
	cu, err := c.customer(ctx, customerID)
	if err != nil {
		return err
	}
	kept := false
	for _, t := range cu.TaxIDs.Data {
		if taxID != nil && t.Type == taxID.Type && t.Value == taxID.Value && !kept {
			kept = true
			continue
		}
		var deleted struct{ ID string }
		if err := c.do(ctx, http.MethodDelete, "/v1/customers/"+url.PathEscape(customerID)+"/tax_ids/"+url.PathEscape(t.ID), "", nil, &deleted); err != nil {
			return err
		}
	}
	if taxID == nil || kept {
		return nil
	}
	var created struct{ ID string }
	return c.do(ctx, http.MethodPost, "/v1/customers/"+url.PathEscape(customerID)+"/tax_ids", key, url.Values{"type": {taxID.Type}, "value": {taxID.Value}}, &created)
}

type stripeCollection struct {
	ID               string `json:"id"`
	CollectionMethod string `json:"collection_method"`
	DaysUntilDue     int    `json:"days_until_due"`
}

func (s stripeCollection) collection() Collection {
	if s.CollectionMethod == "send_invoice" {
		return Collection{PayByInvoice: true, DaysUntilDue: s.DaysUntilDue}
	}
	return Collection{}
}

func (c HTTPStripeClient) GetCollection(ctx context.Context, subscriptionID string) (Collection, error) {
	var out stripeCollection
	if err := c.do(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(subscriptionID), "", nil, &out); err != nil {
		return Collection{}, err
	}
	if out.ID != subscriptionID {
		return Collection{}, errors.New("invalid stripe subscription response")
	}
	return out.collection(), nil
}

// SetCollection switches a subscription between card payment and pay by invoice. Stripe emails
// each invoice to the customer's billing email with a hosted payment page.
func (c HTTPStripeClient) SetCollection(ctx context.Context, subscriptionID string, in Collection, key string) (Collection, error) {
	v := url.Values{"collection_method": {"charge_automatically"}}
	if in.PayByInvoice {
		v = url.Values{"collection_method": {"send_invoice"}, "days_until_due": {strconv.Itoa(in.DaysUntilDue)}}
	}
	var out stripeCollection
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(subscriptionID), key, v, &out); err != nil {
		return Collection{}, err
	}
	if out.ID != subscriptionID || out.collection().PayByInvoice != in.PayByInvoice {
		return Collection{}, errors.New("stripe subscription collection response does not match request")
	}
	return out.collection(), nil
}
