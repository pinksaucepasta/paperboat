package main

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"net/http"
	"strings"
	"syscall"
	"testing"
)

func TestAccessTunnelCommandOwnerClassifiesOnlyCompleteExpectedCauses(t *testing.T) {
	for _, test := range []struct {
		status int
		kind   commandFailureKind
	}{
		{http.StatusUnauthorized, commandRejected},
		{http.StatusForbidden, commandRejected},
		{http.StatusNotFound, commandRejected},
		{http.StatusServiceUnavailable, commandOperational},
		{http.StatusInternalServerError, commandOperational},
	} {
		err := accessTunnelHTTPError(test.status)
		failure := classifyCommandFailure(err)
		if failure.kind != test.kind {
			t.Fatalf("status %d classified as %v, want %v", test.status, failure.kind, test.kind)
		}
		if text := commandFailureMessage(err, failure); text == "" || !strings.Contains(text, "retry") {
			t.Fatalf("status %d has no recovery guidance: %q", test.status, text)
		}
		mixed := errors.Join(err, syscall.EIO)
		if failure := classifyCommandFailure(mixed); failure.kind != commandUnexpected || !errors.Is(mixed, syscall.EIO) {
			t.Fatal("independent operational cause was suppressed by private-access classification")
		}
	}
}

func TestEnvironmentLaunchDenialPresentationIsActionableAndPrivate(t *testing.T) {
	err := &localapi.RemoteError{StatusCode: 503, Code: "environment_unavailable", Message: "private ENV value"}
	for _, message := range []string{commandFailureMessage(err, classifyCommandFailure(err)), safeExecError(err)} {
		if !strings.Contains(message, "ENV delivery") || strings.Contains(message, "private ENV") {
			t.Fatalf("message=%q", message)
		}
	}
	if publicExecErrorCode("environment_unavailable") != "environment_unavailable" {
		t.Fatal("machine code discarded")
	}
}

// A session hostd forgot (heartbeat 404/410, or the command's own select) was the one access outcome the classifier
// did not list, so it ended as an unexpected failure with no guidance.
func TestAccessTunnelGoneIsOperationalWithGuidance(t *testing.T) {
	for _, err := range []error{ErrAccessTunnelGone, accessTunnelFailure(ErrAccessTunnelGone, accessTunnelHTTPStatus{status: http.StatusGone})} {
		failure := classifyCommandFailure(err)
		if failure.kind != commandOperational {
			t.Fatalf("%v classified as %v", err, failure.kind)
		}
		if text := commandFailureMessage(err, failure); !strings.Contains(text, "Run the command again") {
			t.Fatalf("message=%q", text)
		}
	}
}

// soleCommandOwner walked past accessTunnelCauseError to its HTTP status leaf, so every refusal hostd sent
// (403 for a tunnel without a tcp_private route, 401, 404, 503, or an unknown status) printed the generic text.
func TestAccessTunnelRefusalsNameTheirCause(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{accessTunnelHTTPError(http.StatusUnauthorized), "authentication is required"},
		{accessTunnelHTTPError(http.StatusForbidden), "tcp_private"},
		{accessTunnelHTTPError(http.StatusNotFound), "does not support private TCP access"},
		{accessTunnelHTTPError(http.StatusServiceUnavailable), "private access is unavailable"},
		{accessTunnelHTTPError(http.StatusConflict), "with HTTP 409"},
		{accessTunnelFailure(ErrAccessTunnelInvalid, errAccessTunnelMalformedAnswer), "does not understand"},
	} {
		text := commandFailureMessage(test.err, classifyCommandFailure(test.err))
		if !strings.Contains(text, test.want) || strings.Contains(text, "could not finish the command") {
			t.Fatalf("%v: message=%q, want %q", test.err, text, test.want)
		}
	}
}
