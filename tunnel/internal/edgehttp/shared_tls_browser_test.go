package edgehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
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
			response, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Do(request)
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
			if host == "application.test" {
				authority.set(httpDecision)
			}
			authority.unavailable.Store(true)
			manager, err := NewPublicTCPListeners(PublicTCPListenerConfig{ListenHost: "127.0.0.1", InfrastructureHostname: "infrastructure.test", Authority: authority, Routes: registry, MaximumConnections: 4, PreviewHTTPSAuthority: func(context.Context, string) error { return control.ErrBrowserDenied }})
			if err != nil {
				raw.Close()
				t.Fatal(err)
			}
			shared, err := manager.WrapTLSListener(raw, func(name string) bool { return name == host }, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer shared.Close()
			server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ready") })}
			defer server.Close()
			go server.ServeTLS(shared, "", "")
			transport := &http.Transport{TLSClientConfig: clientTLS, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, raw.Addr().String())
			}}
			defer transport.CloseIdleConnections()
			dial := func() error {
				response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get("https://" + host + "/.well-known/paperboat-installation")
				if err != nil {
					return err
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					return err
				}
				if response.StatusCode != 200 || string(body) != "ready" {
					return errors.New("infrastructure HTTPS handler not reached")
				}
				return nil
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
	testSharedTLSAdmittedCarrierAuthority(t, "runtime")
}

func TestSharedTLSRestrictedPreviewCarrierAuthority(t *testing.T) {
	for _, mode := range []string{"private", "team", "public", "unadmitted"} {
		t.Run(mode, func(t *testing.T) { testSharedTLSAdmittedCarrierAuthority(t, mode) })
	}
}

func testSharedTLSAdmittedCarrierAuthority(t *testing.T, mode string) {
	t.Helper()

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
	value := DataCarrierPreviewRoute{RouteID: "runtime_route", Hostname: host, Kind: runtimewire.RouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: identity, Server: carrier, ExpiresAt: time.Now().Add(150 * time.Millisecond)}
	if mode == "runtime" || mode == "unadmitted" {
		if mode == "unadmitted" {
			value.Kind = dataCarrierPreviewPrivateRouteKind
			value.AccessMode = "private"
			value.AttachmentGeneration = 0
		}
		if err = registry.Attach(value); err != nil {
			t.Fatal(err)
		}
	} else {
		d := publicTCPDecision(time.Now().UTC(), 443)
		d.Binding.AccountID, d.Binding.HostID, d.Binding.TunnelID = identity.AccountID, identity.HostID, identity.TunnelID
		d.ConnectorID, d.SessionID = identity.ConnectorID, identity.SessionID
		d.ProcessGeneration, d.ConfigGeneration = identity.ProcessGeneration, identity.Generation
		d.EdgeProcessEpoch, d.Binding.Hostname = value.EdgeProcessEpoch, host
		d.Binding.RouteID, d.Binding.RouteGeneration, d.Binding.PublicationID = value.RouteID, value.Revision, "preview_tls"
		d.ExpiresAt = value.ExpiresAt
		admission := testPreviewCarrierAdmission(d, mode)
		if err = registry.AttachAdmission(admission, carrier); err != nil {
			t.Fatal(err)
		}
	}
	durable, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	defer durable.Close()
	authority := &sharedTLSOutageAuthority{}
	matcher := NewPreviewCarrierRouteMatcher(registry)
	shared := newSharedTLSListener(raw, authority, durable, matcher.HasHTTPSHostname, make(chan struct{}, 4), "infrastructure.test", matcher.HasAdmittedHTTPSHostname, nil, nil)
	assertSharedTLSCarrierAuthority(t, raw, shared, authority, clientTLS, serverTLS, value.ExpiresAt,
		mode != "unadmitted" && mode != "public", func() error {
			return registry.Detach(value.RouteID, identity, value.Revision)
		})
}

func assertSharedTLSCarrierAuthority(t *testing.T, raw net.Listener, shared net.Listener,
	authority *sharedTLSOutageAuthority, clientTLS, serverTLS *tls.Config,
	expiresAt time.Time, allowed bool, revoke func() error) {
	t.Helper()
	var err error
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
	if !allowed {
		if err = dial(); err == nil {
			t.Fatal("resource without current TLS ownership accepted TLS")
		}
		return
	}
	if err = dial(); err != nil {
		t.Fatal("current admitted carrier rejected before HTTP", err)
	}
	if !expiresAt.IsZero() {
		time.Sleep(time.Until(expiresAt) + 10*time.Millisecond)
		if err = dial(); err != nil {
			t.Fatal("admitted carrier lost TLS ownership at credential expiry", err)
		}
	}
	authority.unavailable.Store(true)
	if err = dial(); err == nil {
		t.Fatal("admitted resource accepted without current ingress authority")
	}
	authority.unavailable.Store(false)
	if err = dial(); err != nil {
		t.Fatal("admitted resource did not recover", err)
	}
	if err = revoke(); err != nil {
		t.Fatal(err)
	}
	if err = dial(); err == nil {
		t.Fatal("withdrawn resource accepted TLS")
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
			// The second request is made after the handshake returns to the client; wait for it, then
			// require that no more arrive.
			for deadline := time.Now().Add(5 * time.Second); calls.Load() < wantCalls && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			if calls.Load() != wantCalls {
				t.Fatalf("authority requests=%d want%d", calls.Load(), wantCalls)
			}
		})
	}
}

func TestSharedTLSRestrictedDurableCarrierAuthority(t *testing.T) {
	for _, mode := range []string{"private", "team", "public", "unadmitted", "foreign_config", "withdrawn", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			const host = "service.customer.test"
			clientTLS, serverTLS, _, _, _ := task24Certificates(t, host)
			clientTLS.ServerName = host
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			rules := route.NewRegistry("preview.example.test", "runtime.example.test")
			defer rules.Close()
			rule := replicaRule()
			rule.Hostname, rule.PathPrefix, rule.MatchType = host, "/application", route.MatchExact
			rule.Target, rule.OriginScheme, rule.AccessMode = rule.RouteID, "http", mode
			if mode == "unadmitted" || mode == "foreign_config" || mode == "withdrawn" || mode == "unbound" {
				rule.AccessMode = "private"
			}
			if mode == "withdrawn" {
				rule.DesiredState = "disabled"
			}
			rule.Revision = rule.RouteGeneration
			if err = rules.ApplyGeneration(t.Context(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, 0); err != nil {
				t.Fatal(err)
			}
			durable, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			identity := replicaIdentity(rule.HostID, rule.ConnectorID, rule.ConnectorSessionID, rule.ConfigGeneration)
			carrier, _ := testEdgePreviewCarrierPair(t, identity)
			state := replicaState(rule, rule.ConfigGeneration, rule.AssignmentID, "zone_a", time.Millisecond, 4)
			if mode == "foreign_config" {
				state.RouteBindings[0].ConfigContentHash = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			}
			if mode == "unbound" {
				if err = durable.AttachBound(carrier, "machine-key", "machine-thumb"); err != nil {
					t.Fatal(err)
				}
			} else if mode != "unadmitted" {
				if err = durable.AttachReplica(carrier, "machine-key", "machine-thumb", state); err != nil {
					t.Fatal(err)
				}
			}
			authority := &sharedTLSOutageAuthority{}
			// Restricted routes have no public forwarding grant; use the same
			// admitted-carrier owner as production assembly.
			shared := newSharedTLSListener(raw, authority, durable, rules.HasHTTPSHostname, make(chan struct{}, 4), "infrastructure.test", func(host string) bool { return durable.HasAdmittedHTTPSHostname(host, rules) }, nil, nil)
			assertSharedTLSCarrierAuthority(t, raw, shared, authority, clientTLS, serverTLS, time.Time{}, mode == "private" || mode == "team", func() error { return durable.Detach(carrier) })
		})
	}
}

type quotaDeniedAuthority struct {
	tlsAuthorityFixture
	denied map[string]bool
}

func (a *quotaDeniedAuthority) QuotaDenied(host string) bool { return a.denied[host] }

// An exhausted workspace's route must still complete TLS so the request can be answered with the
// typed 429; before this a quota-denied name looked like an unknown one and the connection was
// closed during the handshake (live check, 2026-10-20). A name that is simply unknown stays closed.
func TestSharedTLSQuotaDeniedRouteCompletesTLSAndUnknownNameDoesNot(t *testing.T) {
	match, _ := browserTestMatch()
	clientTLS, serverTLS, _, _, _ := task24Certificates(t, match.Host)
	clientTLS.ServerName = match.Host
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	defer registry.Close()
	authority := &quotaDeniedAuthority{denied: map[string]bool{match.Host: true}}
	// The route matcher knows the managed hostname; the snapshot has no decision for it.
	shared, err := NewSharedTLSListener(raw, authority, registry, func(host string) bool { return host == match.Host }, 4, "")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})}
	defer server.Close()
	go server.ServeTLS(shared, "", "")
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("a quota-denied route did not complete TLS: %v", err)
	}
	connection.Close()
	authority.denied = nil
	connection, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), clientTLS)
	if err == nil {
		connection.Close()
		t.Fatal("a route with no decision and no quota denial completed TLS")
	}
}
