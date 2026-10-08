package main

import (
	"context"
	"errors"
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
