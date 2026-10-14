package edgehttp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
)

type inspectorAuthorityStub struct {
	access control.InspectorAccess
	err    error
	calls  int
}

func (s *inspectorAuthorityStub) Authorize(_ context.Context, token, kind, resource, route, action string) (control.InspectorAccess, error) {
	s.calls++
	if token != "iat_test" || kind != "tunnel" || resource != "tunnel_replica" || route != "route_replica" || action != "inspect" {
		return control.InspectorAccess{}, errors.New("unexpected authorization input")
	}
	return s.access, s.err
}

func TestInspectorEdgeAccessTraversesExactOwnerCarrier(t *testing.T) {
	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	edgeCarrier, ownerCarrier := testEdgePreviewCarrierPair(t, identity)
	defer edgeCarrier.Close()
	defer ownerCarrier.Close()
	rule := replicaRule()
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.AttachReplica(edgeCarrier, "key", "thumb", replicaState(rule, 3, rule.AssignmentID, "region_01", time.Millisecond, 2)); err != nil {
		t.Fatal(err)
	}
	preview, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "epoch_0001", MaximumRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer preview.Close()
	authority := &inspectorAuthorityStub{access: control.InspectorAccess{
		CredentialID: "iac_test", OwnerAccountID: identity.AccountID, MachineID: identity.HostID,
		ResourceKind: "tunnel", ResourceID: identity.TunnelID, RouteID: rule.RouteID,
		ResourceGeneration: 1, RouteGeneration: rule.RouteGeneration, TargetGeneration: rule.RouteGeneration, ExpiresAt: time.Now().Add(time.Minute),
		Carrier: control.InspectorCarrier{AccountID: identity.AccountID, MachineID: identity.HostID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, Generation: identity.Generation, RouteID: rule.RouteID, AssignmentID: rule.AssignmentID, AssignmentGeneration: rule.AssignmentGeneration, RouteGeneration: rule.RouteGeneration, ConfigContentHash: rule.ConfigContentHash},
	}}
	observed := 0
	handler := &InspectorEdgeAccess{OnFailure: func(_ context.Context, cause error) {
		observed++
		if !errors.Is(cause, io.ErrUnexpectedEOF) && !errors.Is(cause, io.EOF) {
			t.Errorf("original response cause lost: %v", cause)
		}
	}, Authority: authority, Carriers: registry, PreviewCarriers: preview}
	done := make(chan error, 1)
	go func() {
		for attempt := range 2 {
			stream, open, acceptErr := ownerCarrier.AcceptStream(context.Background())
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			defer stream.Close()
			request, readErr := http.ReadRequest(bufio.NewReader(stream))
			if readErr != nil {
				done <- readErr
				return
			}
			if open.Kind != "inspector_http" || open.RouteID != rule.RouteID || request.URL.Path != "/v1/inspector/records" || request.Header.Get(inspectorGrantHeader) != "iat_test" {
				done <- errors.New("inspector carrier metadata mismatch")
				return
			}
			if attempt == 0 {
				_, readErr = io.WriteString(stream, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nx")
				_ = stream.CloseWrite()
			} else {
				_, readErr = io.WriteString(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 14\r\n\r\n{\"records\":[]}")
			}
			done <- readErr
		}
	}()

	request := httptest.NewRequest(http.MethodGet, "https://app.tunnels.example.test/.paperboat/inspector/v1/records?kind=tunnel&resource=tunnel_replica&route=route_replica", nil)
	request.Header.Set(inspectorGrantHeader, "iat_test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || observed != 1 {
		t.Fatalf("failed response=%d observed=%d", response.Code, observed)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	request = request.Clone(request.Context())
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"records":[]}` || authority.calls != 2 || observed != 1 {
		t.Fatalf("response=%d %q calls=%d", response.Code, response.Body.String(), authority.calls)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInspectorEdgeAccessDeniesBeforeCarrierAndBoundsBody(t *testing.T) {
	registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 1})
	defer registry.Close()
	preview, _ := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "epoch_0001", MaximumRoutes: 1})
	defer preview.Close()
	authority := &inspectorAuthorityStub{err: errors.New("denied")}
	handler := &InspectorEdgeAccess{Authority: authority, Carriers: registry, PreviewCarriers: preview}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "https://app.tunnels.example.test/.paperboat/inspector/v1/records?kind=tunnel&resource=tunnel_replica&route=route_replica", nil))
	if missing.Code != http.StatusUnauthorized || authority.calls != 0 {
		t.Fatalf("missing grant response=%d calls=%d", missing.Code, authority.calls)
	}

	largeRequest := httptest.NewRequest(http.MethodPost, "https://app.tunnels.example.test/.paperboat/inspector/v1/replay", strings.NewReader(strings.Repeat("x", inspectorEdgeRequestLimit+1)))
	largeRequest.Header.Set(inspectorGrantHeader, "iat_test")
	large := httptest.NewRecorder()
	handler.ServeHTTP(large, largeRequest)
	if large.Code != http.StatusBadRequest || authority.calls != 0 {
		t.Fatalf("large response=%d calls=%d", large.Code, authority.calls)
	}
}
