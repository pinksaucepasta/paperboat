package datacarrier

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A machine's latency probe connects and closes without a byte; that must not
// be reported. A peer that sends non-TLS bytes is still a typed failure.
func TestHTTPServiceSilentProbeIsNotAFailure(t *testing.T) {
	_, serverTLS, _ := testEndpointCertificates(t)
	var mu sync.Mutex
	var failures []error
	service, err := NewHTTPService(t.Context(), ServiceConfig{Carrier: testCarrierConfig(1), OnFailure: func(_ context.Context, err error) {
		mu.Lock()
		failures = append(failures, err)
		mu.Unlock()
	}, TCP: &EndpointConfig{Address: "127.0.0.1:0", TLS: serverTLS, PeerBinding: func(tls.ConnectionState) (Identity, error) { return testCarrierIdentity(), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	address := service.TCPAddr().String()
	for range 3 {
		probe, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		_ = probe.Close()
	}
	garbage, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := garbage.Write([]byte("not a TLS client hello at all\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	defer garbage.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		mu.Lock()
		got := append([]error(nil), failures...)
		mu.Unlock()
		if len(got) > 0 || time.Now().After(deadline) {
			if len(got) != 1 || !errors.Is(got[0], errCarrierTLSHandshake) {
				t.Fatalf("failures = %v, want exactly one TLS handshake failure (from the garbage peer, none from probes)", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The observed handshake failure carries the refusal reason the TLS check gave.
func TestHTTPServiceHandshakeFailureNamesTheRefusal(t *testing.T) {
	clientTLS, serverTLS, _ := testEndpointCertificates(t)
	serverTLS.VerifyConnection = func(tls.ConnectionState) error { return RefusalUnknownMachine }
	failures := make(chan error, 4)
	service, err := NewHTTPService(t.Context(), ServiceConfig{Carrier: testCarrierConfig(1), OnFailure: func(_ context.Context, err error) { failures <- err },
		TCP: &EndpointConfig{Address: "127.0.0.1:0", TLS: serverTLS, PeerBinding: func(tls.ConnectionState) (Identity, error) { return testCarrierIdentity(), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	clientTLS.NextProtos = []string{"h2"}
	if connection, err := tls.Dial("tcp", service.TCPAddr().String(), clientTLS); err == nil {
		_, _ = connection.Read(make([]byte, 1)) // TLS 1.3: the client learns of the refusal on its first read
		_ = connection.Close()
	}
	select {
	case got := <-failures:
		if !errors.Is(got, errCarrierTLSHandshake) || !errors.Is(got, RefusalUnknownMachine) {
			t.Fatalf("failure = %v, want the TLS handshake failure naming unknown_machine", got)
		}
		leaf, err := x509.ParseCertificate(clientTLS.Certificates[0].Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if thumbprint, remote := FailurePeer(got); thumbprint != keyThumbprint(leaf.PublicKey.(ed25519.PublicKey)) || remote != "127.0.0.1" {
			t.Fatalf("FailurePeer = %q, %q: want the client key's thumbprint and 127.0.0.1", thumbprint, remote)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("refused handshake was not reported")
	}
}

// failingListener fails its first Accepts with a temporary error.
type failingListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

// One "too many open files" ended the accept loop: the edge took no carrier until it restarted.
func TestHandshakeListenerSurvivesTemporaryAcceptError(t *testing.T) {
	clientTLS, serverTLS, _ := testEndpointCertificates(t)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	failing := &failingListener{Listener: inner}
	failing.failures.Store(3)
	var reported atomic.Int32
	listener := newHandshakeListener(failing, serverTLS, 10*time.Second, func(err error) {
		if errors.Is(err, ErrCarrierHTTPServer) && errors.Is(err, syscall.EMFILE) {
			reported.Add(1)
		}
	})
	defer listener.Close()
	go func() {
		if connection, err := (&tls.Dialer{Config: clientTLS}).DialContext(t.Context(), "tcp", inner.Addr().String()); err == nil {
			_ = connection.Close()
		}
	}()
	connection, err := listener.Accept()
	if err != nil {
		t.Fatalf("a temporary accept error ended the listener: %v", err)
	}
	_ = connection.Close()
	if got := reported.Load(); got != 3 {
		t.Fatalf("server failure reports = %d, want 3", got)
	}
}

func TestHandshakeListenerCloseEndsAcceptBackoff(t *testing.T) {
	_, serverTLS, _ := testEndpointCertificates(t)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	failing := &failingListener{Listener: inner}
	failing.failures.Store(1 << 30)
	listener := newHandshakeListener(failing, serverTLS, time.Second, func(error) {})
	_ = listener.Close()
	select {
	case err := <-listener.failed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("accept loop ended with %v, want closed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("accept loop outlived Close")
	}
}
