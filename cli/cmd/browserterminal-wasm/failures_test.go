package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestBrowserFailuresPreserveRecoveryCodesAndExcludeRemotePayload(t *testing.T) {
	for _, remote := range []string{"credential_expired", "not_found_or_forbidden", "stale_generation", "storage_unavailable", "digest_mismatch", "batch_limit"} {
		code, _ := browserFileFailure(remote)
		if code != remote {
			t.Fatalf("recovery code %q became %q", remote, code)
		}
	}
	for _, remote := range []string{"", "PRIVATE_PAYLOAD", "credential_expired PRIVATE_PAYLOAD", strings.Repeat("x", 1<<20)} {
		code, message := browserFileFailure(remote)
		if code != "file_failed" || strings.Contains(code+message, "PRIVATE_PAYLOAD") || len(message) > 150 {
			t.Fatalf("unsafe remote failure projection: code=%q", code)
		}
	}
}

func TestBrowserConnectionFailureRetainsCauseWithOwnedMessage(t *testing.T) {
	err := &terminalConnectError{state: "reconnecting", code: "connection_lost", message: "Reconnect to resume.", cause: context.DeadlineExceeded}
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "Reconnect to resume." {
		t.Fatal("connection failure lost its cause or public message")
	}
}

// F13-03: the edge closing the terminal at the plan limit is a denial with the plan-limit code (the
// console then stops reconnecting); other closes stay a plain end of stream.
func TestPlanLimitCloseIsADenial(t *testing.T) {
	var denied *terminalConnectError
	if !errors.As(closeFailure("bandwidth_quota_exhausted"), &denied) || denied.state != "denied" || denied.code != "bandwidth_quota_exhausted" || !strings.Contains(denied.message, "Billing") {
		t.Fatalf("got %#v", denied)
	}
	if closeFailure("closed") != io.EOF || closeFailure("") != io.EOF {
		t.Fatal("an ordinary close is not a plain end of stream")
	}
}

func TestAPlanLimitDenialInsideATransportFailureWins(t *testing.T) {
	outer := &terminalConnectError{state: "transport-error", code: "connection_failed", cause: closeFailure("bandwidth_quota_exhausted")}
	if got := innermostConnectError(outer); got.code != "bandwidth_quota_exhausted" || got.state != "denied" {
		t.Fatalf("got %#v", got)
	}
}
