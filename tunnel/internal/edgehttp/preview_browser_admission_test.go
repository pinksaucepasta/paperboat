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
	testPreviewMeteredAdmission(t, false)
}
func TestDataCarrierPreviewPublicAccountingAdmission(t *testing.T) {
	testPreviewMeteredAdmission(t, true)
}
func testPreviewMeteredAdmission(t *testing.T, publicPreview bool) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, decision := browserTestMatch()
	decision.Binding.Lifecycle = connectorprotocol.TunnelEphemeral
	decision.Binding.Hostname = "private.preview.example.test"
	decision.Binding.PublicationID = "preview_private"
	decision.ExpiresAt = time.Now().UTC().Add(4 * time.Second)
	if publicPreview {
		decision.Binding.Audience = "public"
		decision.PrincipalID = ""
		decision.GrantID = ""
		decision.GrantGeneration = 0
		decision.MembershipGeneration = 0
	}
	identity := datacarrier.Identity{AccountID: decision.Binding.AccountID, HostID: decision.Binding.HostID, TunnelID: decision.Binding.TunnelID, ConnectorID: decision.ConnectorID, SessionID: decision.SessionID, ProcessGeneration: decision.ProcessGeneration, Generation: decision.ConfigGeneration}
	public := sha256.Sum256([]byte("browser-admission-test-machine"))
	thumb := sha256.Sum256(public[:])
	admission := datacarrier.ExpectedAdmission{Schema: datacarrier.PreviewCarrierSchema, Kind: datacarrier.PreviewCarrierKind, EdgeNodeID: decision.EdgeNodeID, EdgeProcessEpoch: decision.EdgeProcessEpoch, PreviewID: decision.Binding.PublicationID, OperationID: "operation_private", OwnerMachineID: identity.HostID, OwnerSessionID: "owner_private", Identity: identity, LeaseGeneration: 1, ConfigGeneration: identity.Generation, ConfigContentHash: "sha256:" + strings.Repeat("a", 64), RouteID: decision.Binding.RouteID, AccessMode: datacarrier.PreviewCarrierAccessPrivate, RouteKind: datacarrier.PreviewCarrierPrivateRoute, Hostname: decision.Binding.Hostname, RouteRevision: decision.Binding.RouteGeneration, AttachmentGeneration: 1, Endpoint: "https://" + decision.Binding.Hostname, ExpiresAt: decision.ExpiresAt, MachineIdentityPublicKey: base64.RawURLEncoding.EncodeToString(public[:]), MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(thumb[:]), EdgeCarrierServerSPKISHA256: "sha256:" + strings.Repeat("b", 64), EdgeCarrierServerCertificateChainPEM: "test public certificate", Admitted: true}
	if publicPreview {
		admission.AccessMode = "public"
		admission.RouteKind = dataCarrierPreviewRouteKind
	}
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
	if e = registry.Attach(DataCarrierPreviewRoute{RouteID: admission.RouteID, Hostname: admission.Hostname, Kind: admission.RouteKind, AccessMode: admission.AccessMode, EdgeNodeID: decision.EdgeNodeID, EdgeProcessEpoch: decision.EdgeProcessEpoch, Revision: admission.RouteRevision, Server: server, PreviewID: admission.PreviewID, OperationID: admission.OperationID, OwnerMachineID: identity.HostID, OwnerSessionID: admission.OwnerSessionID, LeaseGeneration: 1, AttachmentGeneration: 1, ConfigContentHash: admission.ConfigContentHash, Endpoint: admission.Endpoint, ExpiresAt: admission.ExpiresAt, MachineIdentityPublicKey: admission.MachineIdentityPublicKey, MachineIdentityThumbprint: admission.MachineIdentityThumbprint}); e != nil {
		t.Fatal(e)
	}
	recorder := &ingressUsageRecorder{}
	metering, e := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 1, IngressLimits: testIngressLimits(1, 1<<20), Usage: recorder})
	if e != nil {
		t.Fatal(e)
	}
	defer metering.Close()
	config := DataCarrierPreviewTransportConfig{Registry: registry, IngressRegistry: metering}
	if publicPreview {
		config.PublicAuthority = func(context.Context, DataCarrierPreviewRoute) (connectorprotocol.IngressDecision, error) {
			return decision, nil
		}
	}
	transport, e := NewDataCarrierPreviewTransport(config)
	if e != nil {
		t.Fatal(e)
	}
	makeRequest := func(d *connectorprotocol.IngressDecision) *http.Request {
		r := httptest.NewRequest("GET", admission.Endpoint+"/", nil).WithContext(ctx)
		if publicPreview {
			transport.publicAuthority = func(context.Context, DataCarrierPreviewRoute) (connectorprotocol.IngressDecision, error) {
				if d == nil {
					return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
				}
				return *d, nil
			}
		}
		if d != nil && !publicPreview {
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
	recorder.mu.Lock()
	rejectedRecords := len(recorder.records)
	recorder.mu.Unlock()
	if rejectedRecords != 0 {
		t.Fatal("denied request produced usage")
	}
	done := make(chan error, 1)
	go func() {
		stream, open, e := client.AcceptStream(ctx)
		if e != nil {
			done <- e
			return
		}
		defer stream.Close()
		if !publicPreview {
			current, re := connectorprotocol.ReadIngressDecision(stream, time.Now())
			e = re
			if e == nil {
				e = decision.Authorize(current, open, decision.EdgeNodeID, decision.EdgeProcessEpoch, time.Now())
			}
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
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	var upload, download uint64
	for _, record := range recorder.records {
		if record.environment != decision.Binding.EnvironmentID || record.route != decision.Binding.RouteID || record.revision != decision.Binding.RouteGeneration {
			t.Fatal("usage escaped exact preview binding")
		}
		upload += record.ingress
		download += record.egress
	}
	out := makeRequest(&decision).Clone(ctx)
	out.RequestURI = ""
	out.URL.Scheme = ""
	out.URL.Host = ""
	out.Host = admission.Hostname
	var encoded strings.Builder
	if e := out.Write(&encoded); e != nil {
		t.Fatal(e)
	}
	if upload != uint64(encoded.Len()) || download != uint64(len("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")) {
		t.Fatalf("metered application bytes upload=%d download=%d; preface must be excluded", upload, download)
	}
	lease, e := metering.AcquireIngress(ctx, decision)
	if e != nil {
		t.Fatalf("response close retained capacity: %v", e)
	}
	lease.Release()
}
