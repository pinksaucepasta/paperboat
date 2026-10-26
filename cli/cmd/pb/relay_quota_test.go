package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

const relayQuotaMessage = "This workspace's hosted relay and edge bandwidth is used up. Direct machine-to-machine access still works. Add bandwidth: https://console.example.test/billing"

// F13-03: a command that needed the hosted relay while the allowance is used up says so, with the
// action, instead of a generic transport failure. The daemon reports it as a typed local API error
// for every peer-backed command (exec, ssh/scp/sftp/rsync, terminal, send, ping, preview).
func TestRelayQuotaRefusalNamesThePlanLimitAndTheAction(t *testing.T) {
	refusal := &localapi.RemoteError{StatusCode: http.StatusTooManyRequests, Code: localapi.RelayQuotaCode, Message: relayQuotaMessage}
	err := fmt.Errorf("open exec stream: %w", refusal)
	failure := classifyCommandFailure(err)
	if message := commandFailureMessage(err, failure); message != relayQuotaMessage {
		t.Fatalf("message = %q", message)
	}
	if failure.kind != commandRejected {
		t.Fatalf("kind = %v: a plan limit is a refusal, not a retryable transport failure", failure.kind)
	}
	if retryableInitialConnectFailure(err) {
		t.Fatal("a plan limit must not be retried as a transient connection failure")
	}
	encoded := classifyCLIJSONError(err)
	if encoded.Code != localapi.RelayQuotaCode || encoded.StateChanged != false || encoded.Retryable || !strings.Contains(encoded.Message, "Add bandwidth") || !strings.Contains(encoded.Recovery, "Billing") {
		t.Fatalf("json = %#v", encoded)
	}
	if safeExecError(refusal) != relayQuotaMessage {
		t.Fatalf("exec message = %q", safeExecError(refusal))
	}
	other := &localapi.RemoteError{StatusCode: http.StatusServiceUnavailable, Code: "peer_stream_unavailable", Message: "peer stream could not open"}
	if strings.Contains(commandFailureMessage(other, classifyCommandFailure(other)), "bandwidth") || errors.Is(other, refusal) {
		t.Fatal("other peer failures must not read as a plan limit")
	}
}

// F18-06: a machine outside the API token's scope is a refusal with its own message and code.
func TestAPITokenScopeRefusalSaysTheTokenDoesNotCoverIt(t *testing.T) {
	message := api.PublicMessage(localapi.TokenScopeCode)
	refusal := &localapi.RemoteError{StatusCode: http.StatusForbidden, Code: localapi.TokenScopeCode, Message: message}
	err := fmt.Errorf("open exec stream: %w", refusal)
	if got := commandFailureMessage(err, classifyCommandFailure(err)); got != message || !strings.Contains(got, "scope") {
		t.Fatalf("message = %q", got)
	}
	if retryableInitialConnectFailure(err) {
		t.Fatal("a scope refusal must not be retried")
	}
	if encoded := classifyCLIJSONError(err); encoded.Code != localapi.TokenScopeCode || encoded.Retryable || encoded.StateChanged != false {
		t.Fatalf("json = %#v", encoded)
	}
	if safeExecError(refusal) != message {
		t.Fatalf("exec message = %q", safeExecError(refusal))
	}
	direct := &api.APIError{Status: http.StatusForbidden, Code: localapi.TokenScopeCode}
	if encoded := classifyCLIJSONError(direct); encoded.Code != localapi.TokenScopeCode {
		t.Fatalf("a direct API refusal keeps its code: %#v", encoded)
	}
}
