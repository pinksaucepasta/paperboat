package selfhost

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicIngressAddressBoundary(t *testing.T) {
	for _, text := range []string{"127.0.0.1", "10.1.2.3", "100.64.0.1", "169.254.169.254", "192.0.2.1", "198.18.0.1", "224.0.0.1", "240.0.0.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "2001:db8::1"} {
		if publicIngressIP(netip.MustParseAddr(text)) {
			t.Fatalf("non-public probe address accepted: %s", text)
		}
	}
	for _, text := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIngressIP(netip.MustParseAddr(text)) {
			t.Fatalf("public address rejected: %s", text)
		}
	}
}

func TestPublicIngressRejectsSelfSignedHTTPSBeforeNodeProof(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	t.Cleanup(server.Close)
	transport := publicIngressTransport("example.com", netip.MustParseAddr("127.0.0.1"))
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	_, err := client.Get("https://example.com/.well-known/paperboat-installation")
	var untrusted x509.UnknownAuthorityError
	if !errors.As(err, &untrusted) {
		t.Fatalf("untrusted HTTPS certificate denial = %T %v", err, err)
	}
	if requests.Load() != 0 {
		t.Fatal("public ingress reached node proof over untrusted HTTPS")
	}
}

// pinnedProbe runs probeIngress against server with the pinned-identity transport VerifyIngress uses, minus the
// fixed port 443.
func pinnedProbe(t *testing.T, serverURL string, pin []byte) error {
	t.Helper()
	address := strings.TrimPrefix(strings.TrimPrefix(serverURL, "https://"), "http://")
	_, _, err := probeIngress(t.Context(), "127.0.0.1", "node_a", "secret", func(netip.Addr) bool { return true }, func(string, netip.Addr) *http.Transport {
		return PinnedTransport(pin, address)
	})
	return err
}

// Managed (global) edges serve the operator's publicly trusted certificate, not the enrolled identity: they are
// verified by public trust, never by the pin. Customer scopes stay pinned, so a publicly trusted certificate
// that is not the enrolled identity is refused for them.
func TestIngressTransportTrustFollowsTheInstallationScope(t *testing.T) {
	server := httptest.NewUnstartedServer(challengeHandler("node_a", "secret"))
	server.EnableHTTP2 = false
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	spki := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	wrong := sha256.Sum256([]byte("another identity"))
	probe := func(scope string, pin []byte, trusted bool) error {
		_, _, err := probeIngress(t.Context(), "127.0.0.1", "node_a", "secret", func(netip.Addr) bool { return true }, func(host string, ip netip.Addr) *http.Transport {
			transport := ingressTransportFor(scope, pin, host, ip)
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			if trusted {
				transport.TLSClientConfig.RootCAs = roots
			}
			return transport
		})
		return err
	}
	if err := probe("global", wrong[:], true); err != nil {
		t.Fatalf("a managed edge with a publicly trusted certificate was refused: %v", err)
	}
	if err := probe("global", wrong[:], false); err == nil {
		t.Fatal("a managed edge with an untrusted certificate was accepted")
	}
	for _, scope := range []string{"account", "team"} {
		if err := probe(scope, wrong[:], true); err == nil {
			t.Fatalf("%s scope accepted a certificate that is not the enrolled identity", scope)
		}
		if err := probe(scope, spki[:], false); err != nil {
			t.Fatalf("%s scope refused its enrolled identity: %v", scope, err)
		}
	}
}

func challengeHandler(node, credential string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenge := r.URL.Query().Get("challenge")
		mac := hmac.New(sha256.New, []byte(credential))
		mac.Write([]byte("paperboat-installation/v1\n" + node + "\n" + challenge))
		_ = json.NewEncoder(w).Encode(map[string]string{"node_id": node, "challenge": challenge, "proof": base64.RawURLEncoding.EncodeToString(mac.Sum(nil))})
	})
}

// A self-hosted node serves its enrolled (self-signed) identity and no publicly trusted certificate: the probe
// accepts exactly that identity, and fails closed on any other certificate or on plain HTTP.
func TestPinnedIngressProbeAcceptsOnlyTheEnrolledIdentity(t *testing.T) {
	server := httptest.NewUnstartedServer(challengeHandler("node_a", "secret"))
	server.EnableHTTP2 = false
	server.StartTLS()
	t.Cleanup(server.Close)
	spki := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	if err := pinnedProbe(t, server.URL, spki[:]); err != nil {
		t.Fatalf("enrolled identity without a public certificate was refused: %v", err)
	}
	wrong := sha256.Sum256([]byte("another identity"))
	if err := pinnedProbe(t, server.URL, wrong[:]); err == nil {
		t.Fatal("a certificate that is not the enrolled identity was accepted")
	}
	plain := httptest.NewServer(challengeHandler("node_a", "secret"))
	t.Cleanup(plain.Close)
	if err := pinnedProbe(t, plain.URL, spki[:]); err == nil {
		t.Fatal("plain HTTP was accepted")
	}
	wrongCredential := httptest.NewUnstartedServer(challengeHandler("node_a", "other credential"))
	wrongCredential.StartTLS()
	t.Cleanup(wrongCredential.Close)
	pin := sha256.Sum256(wrongCredential.Certificate().RawSubjectPublicKeyInfo)
	if err := pinnedProbe(t, wrongCredential.URL, pin[:]); !errors.Is(err, ErrIngress) {
		t.Fatalf("right identity with the wrong node proof was not refused: %v", err)
	}
}

func TestPinnedIngressTransportDialsPort443OfTheProbedAddress(t *testing.T) {
	transport := pinnedIngressTransport(make([]byte, 32), netip.MustParseAddr("203.0.113.9"))
	conn, err := transport.DialContext(t.Context(), "tcp", "ignored.example.test:1")
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "203.0.113.9:443") {
		t.Fatalf("pinned ingress transport did not target 203.0.113.9:443: %v", err)
	}
}
