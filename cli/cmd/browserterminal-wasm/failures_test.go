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

// F13-03 / F3-19: the edge closes with a typed reason (shared/terminalend); the worker maps it to the
// state the console shows, by reason first and by the close number when the reason is empty. A dropped
// network (1006) or a plain close stays an end of stream, which reconnects.
func TestEdgeClosesMapToTypedOutcomes(t *testing.T) {
	for _, c := range []struct {
		code         int
		reason       string
		state, typed string
	}{
		{4403, "bandwidth_quota_exhausted", "denied", "bandwidth_quota_exhausted"},
		{4404, "no_access", "denied", "no_access"},
		{4401, "", "denied", "revoked"},
		{4402, "limit", "denied", "limit"},
		{4409, "ended", "closed", "ended"},
		{4408, "expired", "transport-error", "expired"},
		{4429, "too_many_pending", "transport-error", "too_many_pending"},
		{4503, "machine_unavailable", "transport-error", "machine_unavailable"},
		{4504, "", "transport-error", "edge_unavailable"},
		{4999, "something_new", "transport-error", "protocol"},
	} {
		var got *terminalConnectError
		if !errors.As(closeFailure(c.code, c.reason, true), &got) || got.state != c.state || got.code != c.typed || got.details["close"] != c.code {
			t.Fatalf("close %d %q: got %#v, want state %s code %s", c.code, c.reason, got, c.state, c.typed)
		}
	}
	var quota *terminalConnectError
	if errors.As(closeFailure(4403, "bandwidth_quota_exhausted", true), &quota) && !strings.Contains(quota.message, "Billing") {
		t.Fatal("the bandwidth denial lost its wording")
	}
	if closeFailure(1006, "", false) != io.EOF || closeFailure(0, "", false) != io.EOF || closeFailure(1000, "", true) != io.EOF {
		t.Fatal("a dropped network or a plain close is not a plain end of stream")
	}
}

func TestAPlanLimitDenialInsideATransportFailureWins(t *testing.T) {
	outer := &terminalConnectError{state: "transport-error", code: "connection_failed", cause: closeFailure(4403, "bandwidth_quota_exhausted", true)}
	if got := innermostConnectError(outer); got.code != "bandwidth_quota_exhausted" || got.state != "denied" {
		t.Fatalf("got %#v", got)
	}
}
