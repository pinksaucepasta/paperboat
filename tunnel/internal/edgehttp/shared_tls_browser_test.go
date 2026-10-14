package edgehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
)

func TestSharedTLSBrowserAuthorization(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorized", true: "revoked"}[denied], func(t *testing.T) {
			match, decision := browserTestMatch()
			decision.ExpiresAt = time.Now().Add(8 * time.Second)
			authority := &browserAuthorityTest{decision: decision}
			if denied {
				authority.failure = control.ErrBrowserDenied
			}
			browser := &BrowserAccess{Authority: authority, LoginOrigin: "https://login.example.test"}
			clientTLS, serverTLS, _, _, _ := task24Certificates(t, match.Host)
			clientTLS.ServerName = match.Host
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
			defer registry.Close()
			ingress := &tlsAuthorityFixture{}
			ingress.set(decision)
			// The logical route matcher does not know this customer alias.
			shared, err := NewSharedTLSListener(raw, ingress, registry, func(string) bool { return false }, 4, "")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, finish, ok := browser.authorize(w, r, match)
				defer finish()
				if ok {
					_, _ = io.WriteString(w, "authorized application")
				}
			})}
			defer server.Close()
			go server.ServeTLS(shared, "", "")
			transport := &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, raw.Addr().String())
			}}
			defer transport.CloseIdleConnections()
			request, _ := http.NewRequest("GET", "https://"+match.Host+"/", nil)
			request.AddCookie(&http.Cookie{Name: browserEdgeCookie, Value: "token"})
			response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.ProtoMajor != 2 {
				t.Fatal("browser HTTP2 not preserved")
			}
			if denied {
				if response.StatusCode != 404 {
					t.Fatalf("revoked browser status=%d", response.StatusCode)
				}
			} else if response.StatusCode != 200 || string(body) != "authorized application" {
				t.Fatalf("authorized browser status=%d", response.StatusCode)
			}
			expired := decision
			expired.ExpiresAt = time.Now().Add(-time.Second)
			ingress.set(expired)
			expiredConnection, expiredErr := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), clientTLS)
			if expiredErr == nil {
				expiredConnection.Close()
				t.Fatal("expired customer alias authority accepted TLS")
			}
			ingress.set()
			connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), clientTLS)
			if err == nil {
				connection.Close()
				t.Fatal("withdrawn customer alias accepted TLS")
			}
			unknownTLS := clientTLS.Clone()
			unknownTLS.ServerName = "logical-managed.preview.example.test"
			// Isolate ingress classification from certificate hostname validation.
			unknownTLS.InsecureSkipVerify = true
			connection, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), unknownTLS)
			if err == nil {
				connection.Close()
				t.Fatal("unadvertised managed hostname accepted TLS")
			}

		})
	}
}

type sharedTLSOutageAuthority struct {
	tlsAuthorityFixture
	unavailable atomic.Bool
}

func (a *sharedTLSOutageAuthority) Snapshot(ctx context.Context) ([]connectorprotocol.IngressDecision, error) {
	if a.unavailable.Load() {
		return nil, errors.New("control unavailable")
	}
	return a.tlsAuthorityFixture.Snapshot(ctx)
}
func TestSharedTLSAuthorityOutageAndRecovery(t *testing.T) {
	for _, host := range []string{"infrastructure.test", "application.test"} {
		t.Run(host, func(t *testing.T) {
			clientTLS, serverTLS, _, _, _ := task24Certificates(t, host)
			clientTLS.ServerName = host
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
			defer registry.Close()
			authority := &sharedTLSOutageAuthority{}
			_, httpDecision := browserTestMatch()
			httpDecision.Binding.Hostname = host
			httpDecision.ExpiresAt = time.Now().Add(8 * time.Second)
			authority.set(httpDecision)
			authority.unavailable.Store(true)
			shared, err := NewSharedTLSListener(raw, authority, registry, func(name string) bool { return name == host }, 4, "infrastructure.test")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ready") })}
			defer server.Close()
			go server.ServeTLS(shared, "", "")
			dial := func() error {
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), clientTLS)
				if err == nil {
					conn.Close()
				}
				return err
			}
			err = dial()
			if host == "infrastructure.test" && err != nil {
				t.Fatal("reserved infrastructure unavailable during control outage", err)
			}
			if host == "application.test" && err == nil {
				t.Fatal("dynamic hostname accepted without authority")
			}
			authority.unavailable.Store(false)
			if err = dial(); err != nil {
				t.Fatal("fresh authority did not restore HTTPS", err)
			}
		})
	}
}

func TestSharedTLSReservedIPNoSNIRequiresHTTPSBinding(t *testing.T) {
	for _, bound := range []bool{true, false} {
		t.Run(map[bool]string{true: "bound", false: "unbound"}[bound], func(t *testing.T) {
			clientTLS, serverTLS, _, _, _ := task24Certificates(t)
			// An IP ServerName emits no SNI. Keep exact certificate authentication.
			clientTLS.ServerName = "127.0.0.1"
			clientTLS.InsecureSkipVerify = true
			expected := serverTLS.Certificates[0].Certificate[0]
			clientTLS.VerifyConnection = func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, expected) {
					return errors.New("wrong fixture certificate")
				}
				return nil
			}
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
			defer registry.Close()
			manager, err := NewPublicTCPListeners(PublicTCPListenerConfig{ListenHost: "127.0.0.1", InfrastructureHostname: "140.245.11.41", Authority: &tlsAuthorityFixture{}, Routes: registry, MaximumConnections: 4})
			if err != nil {
				raw.Close()
				t.Fatal(err)
			}
			shared, err := manager.WrapTLSListener(raw, func(host string) bool { return bound && host == "140.245.11.41" }, nil)
			if err != nil {
				raw.Close()
				t.Fatal(err)
			}
			server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })}
			defer server.Close()
			go server.ServeTLS(shared, "", "")
			transport := &http.Transport{TLSClientConfig: clientTLS}
			defer transport.CloseIdleConnections()
			response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get("https://" + raw.Addr().String() + "/")
			if !bound {
				if err == nil {
					response.Body.Close()
					t.Fatal("no-SNI accepted without reserved HTTPS binding")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal("reserved infrastructure HTTPS not forwarded")
			}
		})
	}
}

func TestSharedTLSRuntimeCarrierAuthority(t *testing.T) {
	const host = "machine.runtime.example.test"
	clientTLS, serverTLS, _, _, _ := task24Certificates(t, host)
	clientTLS.ServerName = host
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	identity := testEdgePreviewIdentity(1, 1)
	carrier, _ := testEdgePreviewCarrierPair(t, identity)
	value := DataCarrierPreviewRoute{RouteID: "runtime_route", Hostname: host, Kind: datacarrier.RuntimeCarrierRoute, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: identity, Server: carrier, ExpiresAt: time.Now().Add(time.Minute)}
	if err = registry.Attach(value); err != nil {
		t.Fatal(err)
	}
	durable, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	defer durable.Close()
	authority := &sharedTLSOutageAuthority{}
	matcher := NewPreviewCarrierRouteMatcher(registry)
	shared := newSharedTLSListener(raw, authority, durable, matcher.HasHTTPSHostname, make(chan struct{}, 4), "infrastructure.test", matcher.HasRuntimeHTTPSHostname, nil, nil)
	server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	defer server.Close()
	go server.ServeTLS(shared, "", "")
	dial := func() error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), clientTLS)
		if err == nil {
			conn.Close()
		}
		return err
	}
	if err = dial(); err != nil {
		t.Fatal("current runtime carrier rejected before HTTP", err)
	}
	authority.unavailable.Store(true)
	if err = dial(); err == nil {
		t.Fatal("runtime accepted without current ingress authority")
	}
	authority.unavailable.Store(false)
	if err = dial(); err != nil {
		t.Fatal("runtime did not recover", err)
	}
	if err = registry.Detach(value.RouteID, identity, value.Revision); err != nil {
		t.Fatal(err)
	}
	if err = dial(); err == nil {
		t.Fatal("withdrawn runtime carrier accepted TLS")
	}
}

func TestSharedTLSColdPreviewRefreshesExactAuthority(t *testing.T) {
	for _, mode := range []string{"ready", "withdrawn", "outage", "opaque conflict", "replaced admission", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			const host = "cold.preview.example.test"
			identity := testEdgePreviewIdentity(1, 1)
			carrier, peer := testEdgePreviewCarrierPair(t, identity)
			defer carrier.Close()
			defer peer.Close()
			previews, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "epoch_0001"})
			if err != nil {
				t.Fatal(err)
			}
			defer previews.Close()
			value := DataCarrierPreviewRoute{RouteID: "route_cold", Hostname: host, Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "epoch_0001", Revision: 1, Server: carrier, PreviewID: "preview_cold", ExpiresAt: time.Now().Add(time.Minute)}
			if mode != "unknown" {
				if err := previews.Attach(value); err != nil {
					t.Fatal(err)
				}
			}
			matcher := NewPreviewCarrierRouteMatcher(previews)
			d := publicTCPDecision(time.Now().UTC(), 443)
			d.Binding.Protocol = "http"
			d.Binding.OriginScheme = "http"
			d.Binding.PathPrefix = "/"
			d.Binding.ListenerID = ""
			d.Binding.PublicPort = 0
			d.Binding.Lifecycle = connectorprotocol.TunnelEphemeral
			d.Binding.Hostname = host
			d.Binding.PublicationID = value.PreviewID
			d.Binding.RouteID = value.RouteID
			d.Binding.AccountID = identity.AccountID
			d.Binding.TunnelID = identity.TunnelID
			d.Binding.HostID = identity.HostID
			d.ConnectorID = identity.ConnectorID
			d.SessionID = identity.SessionID
			d.ProcessGeneration = identity.ProcessGeneration
			d.ConfigGeneration = identity.Generation
			var calls atomic.Int32
			controlServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				decisions := []connectorprotocol.IngressDecision{}
				if n > 1 {
					if mode == "outage" {
						w.WriteHeader(503)
						return
					}
					if mode == "ready" || mode == "opaque conflict" || mode == "replaced admission" {
						decisions = append(decisions, d)
					}
					if mode == "opaque conflict" {
						conflict := d
						conflict.Binding.Lifecycle = connectorprotocol.TunnelDurable
						conflict.Binding.Protocol = "tls"
						conflict.Binding.OriginScheme = "tcp"
						conflict.Binding.PathPrefix = ""
						conflict.Binding.ListenerID = "listener_tls_443"
						conflict.Binding.PublicPort = 443
						decisions = append(decisions, conflict)
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions})
			}))
			defer controlServer.Close()
			controlClient, err := control.NewHTTPClient(control.HTTPConfig{BaseURL: controlServer.URL, Client: controlServer.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			authority := &control.IngressAuthority{Client: controlClient, NodeID: d.EdgeNodeID, ProcessEpoch: d.EdgeProcessEpoch}
			if _, err = authority.Snapshot(t.Context()); err != nil {
				t.Fatal(err)
			}
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			routes, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
			defer routes.Close()
			previewHTTPSAuthority := func(ctx context.Context, hostname string) error {
				return matcher.AuthorizePublicHTTPS(ctx, hostname, func(ctx context.Context, admission DataCarrierPreviewRoute) (connectorprotocol.IngressDecision, error) {
					decision, err := authority.ResolvePreview(ctx, admission.PreviewID, admission.Hostname, admission.Revision, connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: admission.Identity.AccountID, TunnelID: admission.Identity.TunnelID, ConnectorID: admission.Identity.ConnectorID, SessionID: admission.Identity.SessionID, ProcessGeneration: admission.Identity.ProcessGeneration, Generation: admission.Identity.Generation, RouteID: admission.RouteID, RequestID: "preview-authority", Kind: "http"})
					if mode == "replaced admission" && err == nil {
						if err := previews.Detach(admission.RouteID, admission.Identity, admission.Revision); err != nil {
							t.Fatal(err)
						}
						if err := previews.Attach(admission); err != nil {
							t.Fatal(err)
						}
					}
					return decision, err
				})
			}
			shared := newSharedTLSListener(raw, authority, routes, matcher.HasHTTPSHostname, make(chan struct{}, 4), "", nil, previewHTTPSAuthority, nil)
			clientTLS, serverTLS, _, _, _ := task24Certificates(t, host)
			clientTLS.ServerName = host
			server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })}
			defer server.Close()
			go server.ServeTLS(shared, "", "")
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), clientTLS)
			if err == nil {
				conn.Close()
			}
			if mode == "ready" && err != nil {
				t.Fatalf("first ready preview TLS handshake failed: %v", err)
			}
			if mode != "ready" && err == nil {
				t.Fatal("denied preview or opaque protocol conflict accepted HTTPS")
			}
			wantCalls := int32(2)
			if mode == "unknown" {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("authority requests=%d want%d", calls.Load(), wantCalls)
			}
		})
	}
}
