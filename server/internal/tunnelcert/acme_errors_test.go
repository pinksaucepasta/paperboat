package tunnelcert

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/crypto/acme"
)

func TestMapACMEErrorPreservesTypedCauseWithoutFormattingDetails(t *testing.T) {
	providerError := &acme.Error{StatusCode: http.StatusBadGateway, ProblemType: "urn:ietf:params:acme:error:serverInternal", Detail: "challenge token private-value"}
	err := mapACMEError(providerError)
	if !errors.Is(err, ErrIssuerUnavailable) || !errors.As(err, &providerError) {
		t.Fatalf("issuer cause was lost: %v", err)
	}
	if strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), providerError.Detail) {
		t.Fatalf("provider detail escaped into error text: %q", err.Error())
	}
	var status interface{ DiagnosticStatus() int }
	if !errors.As(err, &status) || status.DiagnosticStatus() != http.StatusBadGateway {
		t.Fatalf("upstream status was lost: %v", err)
	}
}

func TestMapACMEErrorUsesFiniteDNSProblemType(t *testing.T) {
	dnsFailure := &acme.Error{StatusCode: http.StatusBadRequest, ProblemType: "urn:ietf:params:acme:error:dns", Detail: "secret authorization value"}
	err := mapACMEError(dnsFailure)
	if !errors.Is(err, ErrDNSChallengeUnavailable) || strings.Contains(err.Error(), dnsFailure.Detail) {
		t.Fatalf("DNS rejection was not safely classified: %v", err)
	}
	textOnly := &acme.Error{StatusCode: http.StatusBadRequest, ProblemType: "urn:ietf:params:acme:error:unauthorized", Detail: "DNS challenge failed"}
	if got := mapACMEError(textOnly); !errors.Is(got, ErrIssuerUnavailable) || errors.Is(got, ErrDNSChallengeUnavailable) {
		t.Fatalf("free-form detail selected DNS cause: %v", got)
	}
}

func TestExpectedRetryableProviderOutcomeRequiresKnownSoleACMEResult(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{
			name: "issuer policy rejection",
			err:  mapACMEError(&acme.Error{StatusCode: http.StatusForbidden, ProblemType: "urn:ietf:params:acme:error:unauthorized", Detail: "private provider detail"}),
		},
		{
			name: "DNS policy rejection",
			err:  mapACMEError(&acme.Error{StatusCode: http.StatusBadRequest, ProblemType: "urn:ietf:params:acme:error:dns", Detail: "private DNS detail"}),
		},
		{
			name: "authorization challenge rejection",
			err:  mapACMEError(&acme.AuthorizationError{Identifier: "private.example", Errors: []error{&acme.Error{StatusCode: http.StatusBadRequest, ProblemType: "urn:ietf:params:acme:error:dns", Detail: "private detail"}}}),
		},
		{
			name: "rate limit",
			err:  mapACMEError(&acme.Error{StatusCode: http.StatusTooManyRequests, ProblemType: "urn:ietf:params:acme:error:rateLimited", Detail: "private provider detail"}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !IsExpectedRetryableProviderOutcome(test.err) {
				t.Fatalf("mapped provider outcome was not recognized as expected")
			}
			if IsExpectedRetryableProviderOutcome(fmt.Errorf("provider result: %w", errors.Join(test.err, syscall.EIO))) {
				t.Fatal("mixed provider and operational failure was suppressed")
			}
		})
	}

	upstreamFailure := mapACMEError(&acme.Error{StatusCode: http.StatusBadGateway, ProblemType: "urn:ietf:params:acme:error:serverInternal"})
	if IsExpectedRetryableProviderOutcome(upstreamFailure) || IsExpectedRetryableProviderOutcome(ErrIssuerUnavailable) {
		t.Fatal("upstream or unknown issuer failure was treated as an expected policy result")
	}
}
