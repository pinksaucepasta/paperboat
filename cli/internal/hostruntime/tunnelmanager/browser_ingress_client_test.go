package tunnelmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

type browserTestAuth struct {
	body            []byte
	path, operation string
}

func (a *browserTestAuth) Token(context.Context) (string, error) { return "machine-identity", nil }
func (a *browserTestAuth) Proof(_ context.Context, operation, method, path string, body []byte) ([]byte, error) {
	a.body = append([]byte(nil), body...)
	a.path = path
	a.operation = operation
	return []byte("proof"), nil
}

func browserDecisionFixture(now time.Time) (connectorprotocol.IngressDecision, connectorprotocol.StreamOpen) {
	d := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "env-1", AccountID: "account-1", TunnelID: "tunnel-1", Lifecycle: "durable", ResourceGeneration: 1, RouteID: "route-1", RouteGeneration: 1, TargetID: "route-1", TargetGeneration: 1, HostID: "host-1", InstallationGeneration: 1, Audience: "team", ConnectionMethod: "edge", Protocol: "http", Hostname: "app.example.test", PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: "route-1", PublicationGeneration: 1}, DecisionID: "decision-1", PolicyGeneration: 1, EdgeNodeID: "edge-1", EdgeProcessEpoch: "epoch-1234567890123456789012345678", ConnectorID: "connector-1", SessionID: "session-1", ProcessGeneration: 1, ConfigGeneration: 1, AssignmentGeneration: 1, PrincipalID: "viewer-1", GrantID: "grant-1", GrantGeneration: 1, MembershipGeneration: 1, Action: "view", IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	o := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: d.Binding.AccountID, TunnelID: d.Binding.TunnelID, ConnectorID: d.ConnectorID, SessionID: d.SessionID, ProcessGeneration: 1, Generation: 1, RouteID: d.Binding.RouteID, RequestID: "request-1", Kind: "http_browser"}
	return d, o
}

func TestBrowserIngressMachineProofRefreshAndExactBinding(t *testing.T) {
	testBrowserIngressMachineProofRefreshAndExactBinding(t, false)
}
func TestNativeIngressMachineProofRefreshAndExactBinding(t *testing.T) {
	testBrowserIngressMachineProofRefreshAndExactBinding(t, true)
}
func testBrowserIngressMachineProofRefreshAndExactBinding(t *testing.T, native bool) {
	auth := &browserTestAuth{}
	original, open := browserDecisionFixture(time.Now().Add(-time.Minute))
	if native {
		original.Binding.Audience = "private"
		original.PrincipalID, original.GrantID, original.GrantGeneration, original.MembershipGeneration = "accessor-1", "nonce-1", 3, 0
		q := connectorprotocol.PrivateAccessRequest{AccountID: original.Binding.AccountID, ResourceKind: "tunnel", ResourceID: original.Binding.TunnelID, RouteID: original.Binding.RouteID, Audience: "paperboat-tunnel-http", MachineID: original.PrincipalID, SessionID: "accessor-install-1", InstallationGeneration: 3, ExpiresAt: time.Now().UTC().Add(time.Minute), Nonce: original.GrantID, ConnectorID: original.ConnectorID, CarrierSessionID: original.SessionID, RouteGeneration: 1, SessionGeneration: 1, ProcessGeneration: 1, ConfigGeneration: 1, AssignmentGeneration: 1, EdgeNodeID: original.EdgeNodeID, EdgeProcessEpoch: original.EdgeProcessEpoch, Protocol: "http", Method: "CONNECT", Host: original.Binding.Hostname, Path: "/", IdempotencyKey: "operation-1", RequestID: "request-1", CorrelationID: "correlation-1"}
		original.NativeAuthorization = &connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "signed-native-grant", Request: q}
	}
	current := original
	mutate := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/v1/browser-access/ingress/authorize" || auth.path != r.URL.Path || auth.operation != r.Header.Get("Idempotency-Key") || !bytes.Equal(body, auth.body) || r.Header.Get("X-Paperboat-Machine-Identity") != "machine-identity" || r.Header.Get("X-Paperboat-Machine-Proof") == "" || r.Header.Get("Authorization") != "Bearer machine-identity" {
			t.Error("machine proof is not bound to exact request")
		}
		var submitted struct {
			Decision connectorprotocol.IngressDecision `json:"decision"`
			Open     connectorprotocol.StreamOpen      `json:"open"`
		}
		if json.Unmarshal(body, &submitted) != nil || (submitted.Decision.NativeAuthorization == nil) != (original.NativeAuthorization == nil) || submitted.Decision.NativeAuthorization != nil && *submitted.Decision.NativeAuthorization != *original.NativeAuthorization {
			t.Error("native authorization not sent exactly")
		}
		current = original
		current.IssuedAt = time.Now()
		current.ExpiresAt = current.IssuedAt.Add(10 * time.Second)
		switch mutate {
		case "native-grant":
			evidence := *current.NativeAuthorization
			evidence.Grant = "different-grant"
			current.NativeAuthorization = &evidence
		case "native-expired":
			evidence := *current.NativeAuthorization
			evidence.Request.ExpiresAt = time.Now().Add(-time.Second)
			current.NativeAuthorization = &evidence
		case "principal":
			current.PrincipalID = "other-viewer"
		case "grant":
			current.GrantGeneration++
		case "membership":
			current.MembershipGeneration++
		case "target":
			current.Binding.OriginAddress = "127.0.0.1:3001"
		case "expired":
			current.ExpiresAt = time.Now().Add(-time.Second)
		case "denied":
			w.WriteHeader(404)
			return
		case "redirect":
			http.Redirect(w, r, "https://other.example.test", 302)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if mutate == "oversized" {
			_, _ = io.WriteString(w, strings.Repeat(" ", 65<<10))
			return
		}
		if mutate == "duplicate" || mutate == "duplicate-envelope" {
			raw, _ := json.Marshal(current)
			if mutate == "duplicate" {
				raw = bytes.Replace(raw, []byte(`"principal_id":"`+current.PrincipalID+`"`), []byte(`"principal_id":"`+current.PrincipalID+`","principal_id":"`+current.PrincipalID+`"`), 1)
				_, _ = w.Write(append(append([]byte(`{"data":`), raw...), '}'))
			} else {
				_, _ = io.WriteString(w, `{"data":`+string(raw)+`,"data":`+string(raw)+`}`)
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": current})
	}))
	defer server.Close()
	lookup, err := NewBrowserIngressAuthority(server.URL, auth, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := lookup(context.Background(), open, original); err != nil || got.Authorize(current, open, current.EdgeNodeID, current.EdgeProcessEpoch, time.Now()) != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	modes := []string{"principal", "grant", "membership", "target", "expired", "denied", "redirect", "oversized", "duplicate", "duplicate-envelope"}
	if native {
		modes = append(modes, "native-grant", "native-expired")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			mutate = mode
			if _, err := lookup(context.Background(), open, original); err == nil {
				t.Fatal("accepted invalid authority")
			}
		})
	}
}
