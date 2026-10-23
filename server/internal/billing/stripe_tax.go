package billing

import "net/url"

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
