package datacarrier

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
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
	deadline := time.Now().Add(5 * time.Second)
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
	case <-time.After(5 * time.Second):
		t.Fatal("refused handshake was not reported")
	}
}
