package telemetry

import (
	"strings"
	"testing"
	"time"
)

func TestOpaqueIdentityPreservesUUIDAndExistingBindings(t *testing.T) {
	for _, id := range []string{"5b8a43c1-574f-4c56-94e1-a1adf6fc2c83", "tun_existing_01", "rte_existing_01", "pvc_rte_0123456789abcdef", "rtc_ses_abcdef0123456789", "asn_0123456789abcdef", "1234567890abcdef"} {
		if !SafeOpaqueID(id) {
			t.Errorf("safe identity rejected: %q", id)
		}
		_, err := NewEvent(EventInput{At: time.Now(), Severity: SeverityInfo, Component: DimensionEdge, Name: "edge_request", Code: "ready", Outcome: OutcomeSuccess, Message: "Request completed.", CorrelationID: id, IDs: SafeIDs{AccountID: id, RouteID: id, TunnelID: id, ConnectorID: id, AssignmentID: id, RequestID: id}, Retry: RetryNone})
		if err != nil {
			t.Errorf("event lost safe identity %q: %v", id, err)
		}
	}
}

func TestOpaqueIdentityRejectsSensitiveAndUnboundedValues(t *testing.T) {
	for _, id := range []string{"", "ab", strings.Repeat("a", 129), "customer.example.com", "https://example.com", "a/b", "a:b", "hello world", "request\nforged", "Bearer_abcdef", "pbce_abcdefghijklmnop", "pbnative_abcdefghijklmnop", "PBCE_abcdefghijklmnop", "token_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83", "session_secret", "api_key_abcd", "credential_abcd", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhYmNkIn0.signature", "AKIAABCDEFGHIJKLMNOP", "ghp_abcdefghijklmnopqrstuvwxyz", "xoxb-123456789012", "sk_" + "live_" + "abcdefghijklmnopqrstuvwxyz", "-----BEGIN PRIVATE KEY-----"} {
		if SafeOpaqueID(id) {
			t.Errorf("unsafe identity accepted: %q", id)
		}
	}
}
