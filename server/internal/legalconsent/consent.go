// Package legalconsent owns the wording a buyer agrees to before any payment, and its version.
// The wording is published at the Refund Policy page; change Version whenever Text changes.
package legalconsent

import (
	"crypto/sha256"
	"encoding/hex"
)

// Version is the date the wording was published (the Refund Policy's "updated" date).
const Version = "2026-11-19"

// Text is shown beside the purchase button (console, CLI) and in Stripe Checkout's terms acceptance.
const Text = "Start the service now. I agree to the Terms of Service and the Refund Policy: the plan renews automatically " +
	"until I cancel, cancelling stops the next renewal, and payments are not refunded. I ask for the service to start " +
	"during the 14-day withdrawal period and understand that if I withdraw, I pay for the service up to that point."

// TextSHA256 identifies the exact wording a consent record refers to.
func TextSHA256() string {
	sum := sha256.Sum256([]byte(Text))
	return hex.EncodeToString(sum[:])
}

// InvoiceFooter is printed on every invoice and receipt Stripe sends, so the customer keeps the consent, its
// version and how to cancel on a durable medium (Directive 2011/83/EU art. 8(7)-(8)).
func InvoiceFooter() string {
	return "Accepted at checkout (consent version " + Version + "): " + Text +
		" Cancel any time in the console under Billing; cancelling stops the next renewal."
}
