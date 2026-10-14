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
