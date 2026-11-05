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
