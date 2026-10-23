package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/browseringress"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/privateaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type nativePublisherProof struct {
	proof previewattachment.MachineProof
}

func (p nativePublisherProof) VerifyMachineRequest(context.Context, *http.Request, []byte) (previewattachment.MachineProof, error) {
	return p.proof, nil
}

type nativeIngressCheck func(context.Context, privateaccess.AuthorizeInput) (privateaccess.Decision, error)

func (f nativeIngressCheck) AuthorizeIngress(c context.Context, i privateaccess.AuthorizeInput) (privateaccess.Decision, error) {
	return f(c, i)
}

func TestNativeMachineIngressKeepsOperationalFailureDistinctFromDenial(t *testing.T) {
	now := time.Now().UTC()
	q := connectorprotocol.PrivateAccessRequest{AccountID: "account_1", ResourceKind: "tunnel", ResourceID: "tunnel_1", RouteID: "route_1", Audience: "paperboat-tunnel-http", MachineID: "accessor_1", SessionID: "accessor_install_1", InstallationGeneration: 3, ExpiresAt: now.Add(30 * time.Second), Nonce: "nonce_1", ConnectorID: "connector_1", CarrierSessionID: "carrier_1", RouteGeneration: 2, SessionGeneration: 1, ProcessGeneration: 4, ConfigGeneration: 5, AssignmentGeneration: 6, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_12345678901234567890", Protocol: "http", Method: "CONNECT", Host: "private.example.test", Path: "/", IdempotencyKey: "operation_1", RequestID: "request_1", CorrelationID: "correlation_1"}
	d := connectorprotocol.IngressDecision{NativeAuthorization: &connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "signed_grant", Request: q}, Binding: connectorprotocol.IngressBinding{EnvironmentID: "env_1", AccountID: q.AccountID, TunnelID: q.ResourceID, Lifecycle: "durable", ResourceGeneration: 1, RouteID: q.RouteID, RouteGeneration: 2, TargetID: q.RouteID, TargetGeneration: 2, HostID: "publisher_1", InstallationGeneration: 9, Audience: "private", ConnectionMethod: "edge", Protocol: "http", Hostname: q.Host, PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: q.RouteID, PublicationGeneration: 2}, DecisionID: "decision_1", PolicyGeneration: 1, EdgeNodeID: q.EdgeNodeID, EdgeProcessEpoch: q.EdgeProcessEpoch, ConnectorID: q.ConnectorID, SessionID: q.CarrierSessionID, ProcessGeneration: 4, ConfigGeneration: 5, AssignmentGeneration: 6, PrincipalID: q.MachineID, GrantID: q.Nonce, GrantGeneration: 3, Action: "view", IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: q.AccountID, TunnelID: q.ResourceID, ConnectorID: q.ConnectorID, SessionID: q.CarrierSessionID, ProcessGeneration: 4, Generation: 5, RouteID: q.RouteID, RequestID: "request_1", Kind: "http"}
	if err := d.Validate(now); err != nil {
		t.Fatal(err)
	}
	proof := nativePublisherProof{previewattachment.MachineProof{UserID: q.AccountID, MachineID: d.Binding.HostID, InstallationGeneration: d.Binding.InstallationGeneration}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	reference := supportreport.NewReference()
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "expected denial", err: connectorprotocol.ErrIngressDenied, want: http.StatusNotFound},
		{name: "authority outage", err: errors.Join(syscall.ECONNREFUSED, errors.New("PRIVATE_NATIVE_AUTHORITY")), want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := BrowserAccessHandlers{Machine: proof, Ingress: &browseringress.Service{Native: nativeIngressCheck(func(context.Context, privateaccess.AuthorizeInput) (privateaccess.Decision, error) {
				return privateaccess.Decision{}, tc.err
			})}}
			raw, err := json.Marshal(struct {
				Decision connectorprotocol.IngressDecision `json:"decision"`
				Open     connectorprotocol.StreamOpen      `json:"open"`
			}{d, open})
			if err != nil {
				t.Fatal(err)
			}
			ctx := serverdiagnostics.WithCorrelationID(context.Background(), "correlation_private_native_01")
			ctx = context.WithValue(ctx, supportContextKey{}, supportContext{reference: reference, logger: logger})
			request := httptest.NewRequest(http.MethodPost, "/v1/browser-access/ingress/authorize", bytes.NewReader(raw)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			h.MachineRequest(response, request)
			if response.Code != tc.want {
				t.Fatalf("status/body=%d/%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "PRIVATE_NATIVE_AUTHORITY") {
				t.Fatal("native authorization error exposed arbitrary text")
			}
		})
	}
	for _, value := range []string{"connection_refused", reference, "correlation_private_native_01"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("native authorization log omitted %q: %s", value, logs.String())
		}
	}
}

func TestNativeMachineIngressInitialRefreshPublisherFences(t *testing.T) {
	now := time.Now().UTC()
	q := connectorprotocol.PrivateAccessRequest{AccountID: "account_1", ResourceKind: "tunnel", ResourceID: "tunnel_1", RouteID: "route_1", Audience: "paperboat-tunnel-http", MachineID: "accessor_1", SessionID: "accessor_install_1", InstallationGeneration: 3, ExpiresAt: now.Add(30 * time.Second), Nonce: "nonce_1", ConnectorID: "connector_1", CarrierSessionID: "carrier_1", RouteGeneration: 2, SessionGeneration: 1, ProcessGeneration: 4, ConfigGeneration: 5, AssignmentGeneration: 6, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_12345678901234567890", Protocol: "http", Method: "CONNECT", Host: "private.example.test", Path: "/", IdempotencyKey: "operation_1", RequestID: "request_1", CorrelationID: "correlation_1"}
	d := connectorprotocol.IngressDecision{NativeAuthorization: &connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "signed_grant", Request: q}, Binding: connectorprotocol.IngressBinding{EnvironmentID: "env_1", AccountID: q.AccountID, TunnelID: q.ResourceID, Lifecycle: "durable", ResourceGeneration: 1, RouteID: q.RouteID, RouteGeneration: 2, TargetID: q.RouteID, TargetGeneration: 2, HostID: "publisher_1", InstallationGeneration: 9, Audience: "private", ConnectionMethod: "edge", Protocol: "http", Hostname: q.Host, PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: q.RouteID, PublicationGeneration: 2}, DecisionID: "decision_1", PolicyGeneration: 1, EdgeNodeID: q.EdgeNodeID, EdgeProcessEpoch: q.EdgeProcessEpoch, ConnectorID: q.ConnectorID, SessionID: q.CarrierSessionID, ProcessGeneration: 4, ConfigGeneration: 5, AssignmentGeneration: 6, PrincipalID: q.MachineID, GrantID: q.Nonce, GrantGeneration: 3, Action: "view", IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: q.AccountID, TunnelID: q.ResourceID, ConnectorID: q.ConnectorID, SessionID: q.CarrierSessionID, ProcessGeneration: 4, Generation: 5, RouteID: q.RouteID, RequestID: "request_1", Kind: "http"}
	if err := d.Validate(now); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"initial", "refresh"} {
		t.Run(stage, func(t *testing.T) {
			claimed := d
			if stage == "refresh" {
				claimed.IssuedAt = now.Add(-20 * time.Second)
				claimed.ExpiresAt = now.Add(-10 * time.Second)
			}
			for _, mode := range []string{"allow", "accessor-proof", "wrong-install", "wrong-account", "revoked", "target", "carrier", "assignment", "node", "expired"} {
				t.Run(mode, func(t *testing.T) {
					proof := previewattachment.MachineProof{UserID: q.AccountID, MachineID: d.Binding.HostID, InstallationGeneration: 9}
					if mode == "accessor-proof" {
						proof.MachineID = q.MachineID
					}
					if mode == "wrong-install" {
						proof.InstallationGeneration = 3
					}
					if mode == "wrong-account" {
						proof.UserID = "account_other"
					}
					calls := 0
					h := BrowserAccessHandlers{Machine: nativePublisherProof{proof}, Ingress: &browseringress.Service{Native: nativeIngressCheck(func(_ context.Context, input privateaccess.AuthorizeInput) (privateaccess.Decision, error) {
						calls++
						if input.Grant != "signed_grant" || input.Request != privateaccess.Request(q) || input.Edge.NodeID != q.EdgeNodeID || input.Edge.ProcessEpoch != q.EdgeProcessEpoch {
							t.Fatal("native signed authority changed")
						}
						current := d
						current.IssuedAt = time.Now().UTC()
						current.ExpiresAt = current.IssuedAt.Add(10 * time.Second)
						switch mode {
						case "revoked":
							return privateaccess.Decision{}, connectorprotocol.ErrIngressDenied
						case "target":
							current.Binding.OriginAddress = "127.0.0.1:4000"
						case "carrier":
							current.SessionID = "carrier_other"
						case "assignment":
							current.AssignmentGeneration++
						case "node":
							current.EdgeProcessEpoch = "epoch_other12345678901234567890"
						case "expired":
							current.ExpiresAt = now.Add(-time.Second)
						}
						return privateaccess.Decision{Allowed: true, Ingress: &current}, nil
					})}}
					raw, _ := json.Marshal(struct {
						Decision connectorprotocol.IngressDecision `json:"decision"`
						Open     connectorprotocol.StreamOpen      `json:"open"`
					}{claimed, open})
					recorder := httptest.NewRecorder()
					request := httptest.NewRequest("POST", "/v1/browser-access/ingress/authorize", bytes.NewReader(raw))
					request.Header.Set("Content-Type", "application/json")
					h.MachineRequest(recorder, request)
					if mode == "allow" {
						if recorder.Code != 200 || calls != 1 {
							t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
						}
					} else if recorder.Code == 200 {
						t.Fatal("invalid authority admitted")
					}
				})
			}
		})
	}
}
