package edgehttp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
)

func TestBrowserPreviewTransportUsesExactPrivateAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, decision := browserTestMatch()
	decision.Binding.Lifecycle = connectorprotocol.TunnelEphemeral
	decision.Binding.Hostname = "private.preview.example.test"
	decision.Binding.PublicationID = "preview_private"
	decision.ExpiresAt = time.Now().UTC().Add(4 * time.Second)
	identity := datacarrier.Identity{AccountID: decision.Binding.AccountID, HostID: decision.Binding.HostID, TunnelID: decision.Binding.TunnelID, ConnectorID: decision.ConnectorID, SessionID: decision.SessionID, ProcessGeneration: decision.ProcessGeneration, Generation: decision.ConfigGeneration}
	public := sha256.Sum256([]byte("browser-admission-test-machine"))
	thumb := sha256.Sum256(public[:])
	admission := datacarrier.ExpectedAdmission{Schema: datacarrier.PreviewCarrierSchema, Kind: datacarrier.PreviewCarrierKind, EdgeNodeID: decision.EdgeNodeID, EdgeProcessEpoch: decision.EdgeProcessEpoch, PreviewID: decision.Binding.PublicationID, OperationID: "operation_private", OwnerDeviceID: identity.HostID, OwnerSessionID: "owner_private", Identity: identity, LeaseGeneration: 1, ConfigGeneration: identity.Generation, ConfigContentHash: "sha256:" + strings.Repeat("a", 64), RouteID: decision.Binding.RouteID, AccessMode: datacarrier.PreviewCarrierAccessPrivate, RouteKind: datacarrier.PreviewCarrierPrivateRoute, Hostname: decision.Binding.Hostname, RouteRevision: decision.Binding.RouteGeneration, AttachmentGeneration: 1, Endpoint: "https://" + decision.Binding.Hostname, ExpiresAt: decision.ExpiresAt, MachineIdentityPublicKey: base64.RawURLEncoding.EncodeToString(public[:]), MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(thumb[:]), EdgeCarrierServerSPKISHA256: "sha256:" + strings.Repeat("b", 64), EdgeCarrierServerCertificateChainPEM: "test public certificate", Admitted: true}
	expected, e := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch, MaximumAdmissions: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer expected.Close()
	if e = expected.Replace([]datacarrier.ExpectedAdmission{admission}, time.Now()); e != nil {
		t.Fatal(e)
	}
	local, remote := net.Pipe()
	cfg := datacarrier.DefaultConfig()
	cfg.Identity = identity
	cfg.Authorize = expected
	server, e := datacarrier.NewServer(ctx, remote, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	// The receiving test peer has the same independently selected exact grant;
	// it still validates the ingress decision carried on the stream below.
	receiverCtx := datacarrier.WithPrivateAccessDecision(ctx, datacarrier.PrivateAccessDecision{Allowed: true, ExpiresAt: decision.ExpiresAt, ResourceID: admission.PreviewID, RouteID: admission.RouteID, OperationID: admission.OperationID, CarrierSessionID: identity.SessionID, RouteGeneration: admission.RouteRevision, ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation, Protocol: "http"})
	client, e := datacarrier.NewClient(receiverCtx, local, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	registry, e := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: decision.EdgeProcessEpoch})
	if e != nil {
		t.Fatal(e)
	}
	defer registry.Close()
	if e = registry.Attach(DataCarrierPreviewRoute{RouteID: admission.RouteID, Hostname: admission.Hostname, Kind: dataCarrierPreviewPrivateRouteKind, AccessMode: "private", EdgeNodeID: decision.EdgeNodeID, EdgeProcessEpoch: decision.EdgeProcessEpoch, Revision: admission.RouteRevision, Server: server, PreviewID: admission.PreviewID, OperationID: admission.OperationID, OwnerDeviceID: identity.HostID, OwnerSessionID: admission.OwnerSessionID, LeaseGeneration: 1, AttachmentGeneration: 1, ConfigContentHash: admission.ConfigContentHash, Endpoint: admission.Endpoint, ExpiresAt: admission.ExpiresAt, MachineIdentityPublicKey: admission.MachineIdentityPublicKey, MachineIdentityThumbprint: admission.MachineIdentityThumbprint}); e != nil {
		t.Fatal(e)
	}
	transport, e := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry})
	if e != nil {
		t.Fatal(e)
	}
	makeRequest := func(d *connectorprotocol.IngressDecision) *http.Request {
		r := httptest.NewRequest("GET", admission.Endpoint+"/", nil).WithContext(ctx)
		if d != nil {
			r = r.WithContext(context.WithValue(r.Context(), browserDecisionKey{}, *d))
		}
		return r
	}
	wrong := decision
	wrong.Binding.RouteID = "wrong_route"
	expired := decision
	expired.ExpiresAt = time.Now().Add(-time.Second)
	for _, d := range []*connectorprotocol.IngressDecision{nil, &wrong, &expired} {
		if response, e := transport.RoundTrip(makeRequest(d)); e == nil {
			response.Body.Close()
			t.Fatal("unbound browser decision admitted")
		}
	}
	done := make(chan error, 1)
	go func() {
		stream, open, e := client.AcceptStream(ctx)
		if e != nil {
			done <- e
			return
		}
		defer stream.Close()
		current, e := connectorprotocol.ReadIngressDecision(stream, time.Now())
		if e == nil {
			e = decision.Authorize(current, open, decision.EdgeNodeID, decision.EdgeProcessEpoch, time.Now())
		}
		if e == nil {
			request, re := http.ReadRequest(bufio.NewReader(stream))
			e = re
			if request != nil {
				request.Body.Close()
			}
		}
		if e == nil {
			_, e = io.WriteString(stream, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		}
		done <- e
	}()
	response, e := transport.RoundTrip(makeRequest(&decision))
	if e != nil {
		t.Fatalf("validated browser decision rejected by real admission registry: %v", e)
	}
	raw, e := io.ReadAll(response.Body)
	response.Body.Close()
	if e != nil || string(raw) != "ok" {
		t.Fatalf("response %q %v", raw, e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
