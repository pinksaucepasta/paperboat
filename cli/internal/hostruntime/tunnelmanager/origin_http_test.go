package tunnelmanager

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type originSecrets map[string][]byte

func (s originSecrets) ResolveOriginSecret(_ context.Context, reference string) ([]byte, error) {
	value, ok := s[reference]
	if !ok {
		return nil, errors.New("sensitive backend failure for " + reference)
	}
	return append([]byte(nil), value...), nil
}

type observingOriginSecret struct{ payload []byte }

func (s *observingOriginSecret) ResolveOriginSecret(context.Context, string) ([]byte, error) {
	return s.payload, nil
}

type countingOriginDialer struct{ count atomic.Int32 }

func (d *countingOriginDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.count.Add(1)
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

type originPKI struct {
	caPEM     []byte
	serverTLS tls.Certificate
	clientDoc []byte
}

func newOriginPKI(t *testing.T, serverName string) originPKI {
	t.Helper()
	now := time.Now().UTC()
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Paperboat test origin CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, name string, usage x509.ExtKeyUsage) (tls.Certificate, []byte, []byte) {
		_, key, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
		der, createErr := x509.CreateCertificate(rand.Reader, template, ca, key.Public(), caKey)
		if createErr != nil {
			t.Fatal(createErr)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyBytes, marshalErr := x509.MarshalPKCS8PrivateKey(key)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
		certificate, pairErr := tls.X509KeyPair(certPEM, keyPEM)
		if pairErr != nil {
			t.Fatal(pairErr)
		}
		return certificate, certPEM, keyPEM
	}
	server, _, _ := issue(2, serverName, x509.ExtKeyUsageServerAuth)
	_, clientCert, clientKey := issue(3, "paperboat-origin-client", x509.ExtKeyUsageClientAuth)
	document, err := json.Marshal(originClientIdentity{CertificatePEM: string(clientCert), PrivateKeyPEM: string(clientKey)})
	if err != nil {
		t.Fatal(err)
	}
	return originPKI{caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), serverTLS: server, clientDoc: document}
}

func httpsOriginRoute(address, serverName string) hoststate.TunnelConfigRoute {
	caRef := "keychain://paperboat/origins/ca_01"
	return hoststate.TunnelConfigRoute{ID: "route_01", Protocol: "http", OriginScheme: "https", OriginAddress: address, PreserveHost: true, TLSVerification: "custom_ca", TLSServerName: &serverName, CAReference: &caRef, ConnectTimeoutMs: 1000, IdleTimeoutMs: 2000, DesiredState: "active"}
}

func TestOriginHTTPTransportCustomCAKeepsHostAndSNIIndependent(t *testing.T) {
	pki := newOriginPKI(t, "origin.internal.test")
	seenHost := ""
	seenInternal := ""
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenHost = request.Host
		seenInternal = request.Header.Get("X-Paperboat-Session")
		writer.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverTLS}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	route := httpsOriginRoute(server.Listener.Addr().String(), "origin.internal.test")
	provider := ReferenceOriginTLSProvider{Secrets: originSecrets{*route.CAReference: pki.caPEM}}
	request, _ := http.NewRequest(http.MethodGet, "https://public.example.test/status", nil)
	request.Host = "public.example.test"
	request.Header.Set("X-Paperboat-Session", "must-not-reach-origin")
	response, err := (&OriginHTTPTransport{TLS: provider}).RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || seenHost != "public.example.test" || seenInternal != "" {
		t.Fatalf("status=%d host=%q internal=%q", response.StatusCode, seenHost, seenInternal)
	}
	hostOverride := "backend-vhost.internal.test"
	route.HostOverride = &hostOverride
	request, _ = http.NewRequest(http.MethodGet, "https://public.example.test/status", nil)
	request.Host = "public.example.test"
	response, err = (&OriginHTTPTransport{TLS: provider}).RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if seenHost != hostOverride {
		t.Fatalf("Host=%q, want independent override %q", seenHost, hostOverride)
	}
	wrong := "wrong.internal.test"
	route.TLSServerName = &wrong
	request, _ = http.NewRequest(http.MethodGet, "https://public.example.test/status", nil)
	if _, err := (&OriginHTTPTransport{TLS: provider}).RoundTrip(context.Background(), route, request); !errors.Is(err, ErrOriginUnavailable) {
		t.Fatalf("wrong SNI/hostname error=%v", err)
	}
}

func TestOriginHTTPTransportRequiresConfiguredMTLSIdentity(t *testing.T) {
	pki := newOriginPKI(t, "origin.internal.test")
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pki.caPEM) {
		t.Fatal("append CA")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverTLS}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	route := httpsOriginRoute(server.Listener.Addr().String(), "origin.internal.test")
	clientRef := "keychain://paperboat/origins/mtls_01"
	route.MTLSCredentialReference = &clientRef
	secrets := originSecrets{*route.CAReference: pki.caPEM, clientRef: pki.clientDoc}
	request, _ := http.NewRequest(http.MethodGet, "https://public.example.test/", nil)
	response, err := (&OriginHTTPTransport{TLS: ReferenceOriginTLSProvider{Secrets: secrets}}).RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	delete(secrets, clientRef)
	request, _ = http.NewRequest(http.MethodGet, "https://public.example.test/", nil)
	_, err = (&OriginHTTPTransport{TLS: ReferenceOriginTLSProvider{Secrets: secrets}}).RoundTrip(context.Background(), route, request)
	if err == nil || strings.Contains(err.Error(), clientRef) || strings.Contains(err.Error(), "sensitive backend") {
		t.Fatalf("missing credential error leaked secret context: %v", err)
	}
}

// A self-signed origin (Proxmox, Synology) is trusted by its pinned fingerprint, known_hosts style: a
// different certificate is refused with the re-pin hint and reported as certificate_changed.
func TestOriginHTTPTransportPinnedAndInsecureVerification(t *testing.T) {
	pki := newOriginPKI(t, "origin.internal.test")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverTLS}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	leaf, err := x509.ParseCertificate(pki.serverTLS.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	visit := func(route hoststate.TunnelConfigRoute) error {
		request, _ := http.NewRequest(http.MethodGet, "https://public.example.test/", nil)
		response, err := (&OriginHTTPTransport{}).RoundTrip(context.Background(), route, request)
		if err == nil {
			response.Body.Close()
		}
		return err
	}
	route := httpsOriginRoute(server.Listener.Addr().String(), "wrong.internal.test")
	route.CAReference = nil
	route.TLSVerification = "insecure"
	if err := visit(route); err != nil {
		t.Fatalf("insecure: %v", err)
	}
	pin := CertificateFingerprint(leaf)
	route.TLSVerification, route.TLSPin = "pinned", &pin
	if err := visit(route); err != nil {
		t.Fatalf("pinned to the presented certificate: %v", err)
	}
	other := "sha256:" + strings.Repeat("0", 64)
	route.TLSPin = &other
	err = visit(route)
	var changed *CertificateChangedError
	if !errors.As(err, &changed) || changed.Presented != pin || !strings.Contains(err.Error(), "re-pin") || originHealthCode("", err) != sharedprotocol.OriginCertificateChanged {
		t.Fatalf("pin mismatch error = %v", err)
	}
}

func TestReferenceOriginTLSProviderClearsResolverOwnedBuffer(t *testing.T) {
	pki := newOriginPKI(t, "origin.internal.test")
	secret := &observingOriginSecret{payload: append([]byte(nil), pki.caPEM...)}
	reference := "keychain://paperboat/origins/ca_01"
	_, err := (ReferenceOriginTLSProvider{Secrets: secret}).TLSConfig(context.Background(), hoststate.TunnelConfigRoute{TLSVerification: "custom_ca", CAReference: &reference})
	if err != nil {
		t.Fatal(err)
	}
	for index, value := range secret.payload {
		if value != 0 {
			t.Fatalf("resolver buffer byte %d was not cleared", index)
		}
	}
}

func TestOriginHTTPTransportReusesConnectionsAndGenerationCloseFencesPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte("ok")) }))
	defer server.Close()
	dialer := &countingOriginDialer{}
	base := &OriginHTTPTransport{Dialer: dialer}
	first := base.newGeneration()
	route := hoststate.TunnelConfigRoute{ID: "route_01", Protocol: "http", OriginScheme: "http", OriginAddress: server.Listener.Addr().String(), PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 5000, DesiredState: "active"}
	for range 2 {
		request, _ := http.NewRequest(http.MethodGet, "http://public.example.test/", nil)
		response, err := first.RoundTrip(context.Background(), route, request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if dialer.count.Load() != 1 {
		t.Fatalf("same generation dials=%d, want keepalive reuse", dialer.count.Load())
	}
	first.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, "http://public.example.test/", nil)
	if _, err := first.RoundTrip(context.Background(), route, request); !errors.Is(err, ErrOriginUnavailable) {
		t.Fatalf("closed generation error=%v", err)
	}
	second := base.newGeneration()
	request, _ = http.NewRequest(http.MethodGet, "http://public.example.test/", nil)
	response, err := second.RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	second.CloseIdleConnections()
	if dialer.count.Load() != 2 {
		t.Fatalf("replacement generation dials=%d, want separate policy pool", dialer.count.Load())
	}
}

func TestOriginHTTPTransportUsesRealH2C(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.ProtoMajor != 2 {
			t.Errorf("origin protocol=%s", request.Proto)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetHTTP1(true)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	defer server.Close()
	route := hoststate.TunnelConfigRoute{ID: "route_h2c", Protocol: "http", OriginScheme: "h2c", OriginAddress: server.Listener.Addr().String(), PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 5000, DesiredState: "active"}
	transport := (&OriginHTTPTransport{}).newGeneration()
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, "http://public.example.test/", nil)
	response, err := transport.RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusNoContent {
		t.Fatalf("response protocol/status=%s %d", response.Proto, response.StatusCode)
	}
}

// A strict gRPC origin (grpc-java, Envoy) refuses a request without "te: trailers" and answers over h2c with a
// grpc-status trailer: both must cross the agent's transport, and no other TE value may.
func TestOriginHTTPTransportCarriesGRPCTEHeaderAndTrailersOverH2C(t *testing.T) {
	var sawTE []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sawTE = request.Header.Values("Te")
		if request.ProtoMajor != 2 || request.Header.Get("Te") != "trailers" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "application/grpc")
		writer.Header().Set("Trailer", "Grpc-Status")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte{0, 0, 0, 0, 0})
		writer.Header().Set("Grpc-Status", "0")
	}))
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	defer server.Close()
	route := hoststate.TunnelConfigRoute{ID: "route_grpc", Protocol: "http", OriginScheme: "h2c", OriginAddress: server.Listener.Addr().String(), PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 5000, DesiredState: "active"}
	transport := (&OriginHTTPTransport{}).newGeneration()
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodPost, "http://public.example.test/pkg.Service/Method", bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	request.Header.Set("Content-Type", "application/grpc")
	request.Header.Set("Te", "trailers, deflate") // the edge sends exactly "trailers"; any other token must not pass
	response, err := transport.RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Trailer.Get("Grpc-Status") != "0" || len(sawTE) != 1 || sawTE[0] != "trailers" {
		t.Fatalf("status=%d trailers=%v origin TE=%q", response.StatusCode, response.Trailer, sawTE)
	}
}

func TestOriginHTTPHeaderSanitizationPreservesApplicationCredentials(t *testing.T) {
	request := http.Header{
		"Authorization":               {"Bearer application"},
		"Proxy-Authorization":         {"Bearer proxy"},
		"Forwarded":                   {"for=198.51.100.7;proto=https;host=app.example.test"},
		"X-Forwarded-For":             {"198.51.100.7"},
		"X-Forwarded-Host":            {"app.example.test"},
		"X-Forwarded-Proto":           {"https"},
		"X-Forwarded-Port":            {"443"},
		"X-Real-Ip":                   {"198.51.100.7"},
		"X-Forwarded-Server":          {"attacker"},
		"Paperboat-Visitor-Assertion": {"signed.visitor.jwt"},
		"X-Paperboat-Grant":           {"secret"},
		"Paperboat-Grant":             {"secret"},
		"Cookie":                      {"session=application; Paperboat-access=secret; __Host-Paperboat_csrf=secret; theme=dark"},
	}
	sanitizeOriginRequestHeaders(request, false, false)
	if request.Get("Authorization") != "Bearer application" || request.Get("Cookie") != "session=application; theme=dark" {
		t.Fatalf("application credentials changed: auth=%q cookie=%q", request.Get("Authorization"), request.Get("Cookie"))
	}
	for name, want := range map[string]string{"Forwarded": "for=198.51.100.7;proto=https;host=app.example.test", "X-Forwarded-For": "198.51.100.7", "X-Forwarded-Host": "app.example.test", "X-Forwarded-Proto": "https", "X-Forwarded-Port": "443", "X-Real-IP": "198.51.100.7", "Paperboat-Visitor-Assertion": "signed.visitor.jwt"} {
		if request.Get(name) != want {
			t.Fatalf("edge fact %s = %q, want %q", name, request.Get(name), want)
		}
	}
	for _, name := range []string{"Proxy-Authorization", "X-Forwarded-Server", "X-Paperboat-Grant", "Paperboat-Grant"} {
		if request.Get(name) != "" {
			t.Fatalf("reserved request header %s survived", name)
		}
	}

	response := http.Header{
		"X-Paperboat-Decision": {"secret"},
		"Paperboat-Decision":   {"secret"},
		"Set-Cookie":           {"session=application; Path=/", "Paperboat-access=secret; Path=/", "__Secure-Paperboat_csrf=secret; Path=/"},
	}
	sanitizeOriginResponseHeaders(response, false, false)
	if got := response.Values("Set-Cookie"); len(got) != 1 || got[0] != "session=application; Path=/" {
		t.Fatalf("sanitized response cookies=%q", got)
	}
	if response.Get("X-Paperboat-Decision") != "" || response.Get("Paperboat-Decision") != "" {
		t.Fatalf("reserved response headers survived: %v", response)
	}
}

// A WebSocket handshake keeps the edge's forwarding facts like any request (the edge has already replaced the
// visitor's values); the sanitizer re-adds the upgrade headers and drops transport references and X-Forwarded-Server.
func TestOriginHTTPWebSocketUpgradeKeepsEdgeForwardedFactsOnly(t *testing.T) {
	request := http.Header{
		"Connection":         {"Upgrade, X-Spoof"},
		"Upgrade":            {"websocket"},
		"X-Spoof":            {"1"},
		"Forwarded":          {"for=198.51.100.7;proto=https;host=app.example.test"},
		"X-Forwarded-For":    {"198.51.100.7"},
		"X-Forwarded-Host":   {"app.example.test"},
		"X-Forwarded-Proto":  {"https"},
		"X-Forwarded-Port":   {"443"},
		"X-Real-Ip":          {"198.51.100.7"},
		"X-Forwarded-Server": {"attacker"},
	}
	sanitizeOriginRequestHeaders(request, true, false)
	if request.Get("Connection") != "Upgrade" || request.Get("Upgrade") != "websocket" || request.Get("X-Spoof") != "" {
		t.Fatalf("upgrade headers = %v", request)
	}
	for name, want := range map[string]string{"Forwarded": "for=198.51.100.7;proto=https;host=app.example.test", "X-Forwarded-For": "198.51.100.7", "X-Forwarded-Host": "app.example.test", "X-Forwarded-Proto": "https", "X-Forwarded-Port": "443", "X-Real-IP": "198.51.100.7"} {
		if request.Get(name) != want {
			t.Fatalf("websocket edge fact %s = %q, want %q", name, request.Get(name), want)
		}
	}
	if request.Get("X-Forwarded-Server") != "" {
		t.Fatal("X-Forwarded-Server survived the websocket handshake")
	}
}

func TestOriginHTTPOnlyPreservesValidatedWebSocketUpgrade(t *testing.T) {
	valid, _ := http.NewRequest(http.MethodGet, "http://public.example.test/socket", nil)
	valid.Header.Set("Connection", "keep-alive, Upgrade")
	valid.Header.Set("Upgrade", "WebSocket")
	valid.Header.Set("Sec-WebSocket-Version", "13")
	valid.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if !validWebSocketUpgrade(valid) {
		t.Fatal("valid WebSocket upgrade rejected")
	}
	invalid := valid.Clone(context.Background())
	invalid.Header.Set("Sec-WebSocket-Key", "attacker")
	if validWebSocketUpgrade(invalid) {
		t.Fatal("invalid WebSocket upgrade accepted")
	}
	sanitizeOriginRequestHeaders(invalid.Header, false, false)
	if invalid.Header.Get("Connection") != "" || invalid.Header.Get("Upgrade") != "" {
		t.Fatal("invalid upgrade survived sanitization")
	}
}

// The machine's own runtime listener is served through the same forwarder, and the control plane's
// inbox request carries its operation id in X-Paperboat-Operation-Id and reads the file name from
// X-Paperboat-Inbox-File-Name. A user's origin never sees or sets such headers; the runtime listener must.
func TestOriginHTTPTransportKeepsControlPlaneHeadersOnlyWhenAsked(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = request.Header.Get("X-Paperboat-Operation-Id")
		writer.Header().Set("X-Paperboat-Inbox-File-Name", "report.txt")
		writer.Header().Set("Paperboat-Decision", "internal")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	route := hoststate.TunnelConfigRoute{ID: "route_01", Protocol: "http", OriginScheme: "http", OriginAddress: server.Listener.Addr().String(), PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 2000, DesiredState: "active"}
	for _, keep := range []bool{false, true} {
		seen = ""
		request, _ := http.NewRequest(http.MethodGet, "http://public.example.test/v1/inbox-files", nil)
		request.Header.Set("X-Paperboat-Operation-Id", "inbox-12345678")
		response, err := (&OriginHTTPTransport{KeepControlPlaneHeaders: keep}).RoundTrip(context.Background(), route, request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if keep != (seen == "inbox-12345678") || keep != (response.Header.Get("X-Paperboat-Inbox-File-Name") == "report.txt") {
			t.Fatalf("keep=%v: origin saw operation id %q, response file name %q", keep, seen, response.Header.Get("X-Paperboat-Inbox-File-Name"))
		}
		if response.Header.Get("Paperboat-Decision") != "" {
			t.Fatal("an internal decision header reached the client")
		}
	}
}

// Caddy/Traefik on https://localhost:443 pick the site by SNI: a loopback or IP origin is sent the
// visitor's public hostname, each name on its own pooled transport; a DNS-name origin or an explicit
// tls_server_name keeps its own.
func TestOriginHTTPTransportSendsPublicSNIToLoopbackOrigins(t *testing.T) {
	pki := newOriginPKI(t, "app.example.test")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName != "app.example.test" {
			return nil, errors.New("no site for this name")
		}
		return &pki.serverTLS, nil
	}}
	server.StartTLS()
	defer server.Close()
	route := httpsOriginRoute(server.Listener.Addr().String(), "")
	route.TLSServerName = nil
	transport := &OriginHTTPTransport{TLS: ReferenceOriginTLSProvider{Secrets: originSecrets{*route.CAReference: pki.caPEM}}}
	defer transport.CloseIdleConnections()
	visit := func(publicHost string) error {
		request, _ := http.NewRequest(http.MethodGet, "https://"+publicHost+"/", nil)
		request.Header.Set("X-Forwarded-Host", publicHost)
		response, err := transport.RoundTrip(context.Background(), route, request)
		if err == nil {
			response.Body.Close()
		}
		return err
	}
	if err := visit("app.example.test"); err != nil {
		t.Fatalf("public SNI to a loopback origin: %v", err)
	}
	if err := visit("other.example.test"); err == nil {
		t.Fatal("a second public name reused the first name's TLS session")
	}
	if err := visit("app.example.test"); err != nil {
		t.Fatalf("first name broken by the second: %v", err)
	}
	explicit, dns := "origin.internal.test", "app.internal.test:443"
	for _, test := range []struct {
		route hoststate.TunnelConfigRoute
		want  string
	}{
		{hoststate.TunnelConfigRoute{OriginScheme: "https", OriginAddress: "localhost:443"}, "app.example.test"},
		{hoststate.TunnelConfigRoute{OriginScheme: "https", OriginAddress: "[::1]:8443"}, "app.example.test"},
		{hoststate.TunnelConfigRoute{OriginScheme: "https", OriginAddress: "192.168.1.10:8006"}, "app.example.test"},
		{hoststate.TunnelConfigRoute{OriginScheme: "https", OriginAddress: dns}, ""},
		{hoststate.TunnelConfigRoute{OriginScheme: "https", OriginAddress: "localhost:443", TLSServerName: &explicit}, explicit},
		{hoststate.TunnelConfigRoute{OriginScheme: "http", OriginAddress: "localhost:80"}, ""},
	} {
		got := withPublicServerName(test.route, "app.example.test:443").TLSServerName
		if (got == nil) != (test.want == "") || got != nil && *got != test.want {
			t.Errorf("%s: SNI %v, want %q", test.route.OriginAddress, got, test.want)
		}
	}
}

// With --host-header localhost:3000 the app redirects to and scopes cookies for localhost:3000; the
// visitor gets them back on the public host.
func TestOriginHTTPTransportRestoresPublicHostAfterHostRewrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Add("Set-Cookie", "session=1; Path=/; Domain=localhost; HttpOnly")
		writer.Header().Add("Set-Cookie", "theme=dark; Domain=example.test")
		http.Redirect(writer, request, "http://"+request.Host+"/login?next=%2F", http.StatusFound)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	override := "localhost:" + port
	route := hoststate.TunnelConfigRoute{ID: "route_01", Protocol: "http", OriginScheme: "http", OriginAddress: server.Listener.Addr().String(), HostOverride: &override, TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 2000, DesiredState: "active"}
	request, _ := http.NewRequest(http.MethodGet, "https://app.example.test/", nil)
	request.Header.Set("X-Forwarded-Host", "app.example.test")
	response, err := (&OriginHTTPTransport{}).RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	cookies := response.Header.Values("Set-Cookie")
	if response.Header.Get("Location") != "https://app.example.test/login?next=%2F" || len(cookies) != 2 || strings.Contains(cookies[0], "Domain") || !strings.Contains(cookies[0], "HttpOnly") || !strings.Contains(cookies[1], "Domain=example.test") {
		t.Fatalf("location=%q cookies=%q", response.Header.Get("Location"), cookies)
	}
}
