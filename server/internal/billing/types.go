package billing

import (
	"errors"
	"regexp"
)

var (
	ErrIdempotencyConflict          = errors.New("idempotency key conflicts with existing billing operation")
	ErrUnknownProduct               = errors.New("billing product is not active or mapped")
	ErrCheckoutPending              = errors.New("another billing checkout is already pending")
	ErrProviderOutcomeUnknown       = errors.New("billing provider outcome is unknown")
	ErrBillingRecoveryInvalid       = errors.New("billing recovery request is invalid")
	ErrBillingRecoveryConflict      = errors.New("billing recovery idempotency conflict")
	ErrBillingOperationNotUncertain = errors.New("billing operation is not uncertain")
)

var evidenceReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/-]{7,255}$`)

type CheckoutSession struct {
	URL                string
	ProviderSessionID  string
	ProviderCustomerID string
}

type CustomerPortalSession struct {
	URL string
}
