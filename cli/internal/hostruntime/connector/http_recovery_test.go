package connector

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

// The first attempt sees a real UDP blackhole. Re-establishment after UDP is
// available must select H3 again; existing application streams are never replayed.
func TestHTTPPoolUDPBlackholeFallbackAndPreferredRecovery(t *testing.T) {
	clientTLS, serverTLS, _ := testDataCarrierCertificates(t)
	identity := testDataCarrierIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", 405)
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		link := &recoveryHTTPLink{Reader: r.Body, w: w}
		carrier, err := NewDataCarrierServer(r.Context(), link, testDataCarrierConfig(), DataCarrierAdmission{Identity: identity, Authorize: func(context.Context, StreamOpen) error { return nil }})
		if err != nil {
			return
		}
		defer carrier.Close()
		<-r.Context().Done()
	})
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverTLS.NextProtos = []string{"h2"}
	h2 := &http.Server{Handler: handler, TLSConfig: serverTLS}
	if err := http2.ConfigureServer(h2, &http2.Server{}); err != nil {
		t.Fatal(err)
	}
	go h2.Serve(tls.NewListener(tcp, serverTLS))
	defer h2.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	endpoint := func(address string) DataCarrierEndpointConfig {
		return DataCarrierEndpointConfig{Address: address, TLS: clientTLS, ExpectedIdentity: identity, PeerBinding: func(tls.ConnectionState) (DataCarrierIdentity, error) { return identity, nil }}
	}
	dial := NewHTTPNetworkDialer(NetworkDialerConfig{TCPMux: endpoint(tcp.Addr().String()), QUIC: endpoint(udp.LocalAddr().String())})
	config := DefaultDataCarrierPoolConfig()
	config.MaximumCarriers = 1
	config.Session = identity
	config.Preferred = HTTP3
	config.Fallback = HTTP2
	config.Carrier = testDataCarrierConfig()
	connect := func(want Transport) *DataCarrierPool {
		t.Helper()
		pool, err := NewDataCarrierPool(ctx, config, dial)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pool.Close() })
		if err := pool.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		if got, ok := pool.SelectedTransport(); !ok || got != want {
			t.Fatalf("selected %s, %v; want %s", got, ok, want)
		}
		return pool
	}
	fallback := connect(HTTP2)
	if err := fallback.Close(); err != nil {
		t.Fatal(err)
	}
	h3TLS := serverTLS.Clone()
	h3TLS.NextProtos = []string{"h3"}
	h3 := &http3.Server{Handler: handler, TLSConfig: h3TLS}
	go h3.Serve(udp)
	defer h3.Close()
	connect(HTTP3)
}

type recoveryHTTPLink struct {
	io.Reader
	w http.ResponseWriter
}

func (l *recoveryHTTPLink) Write(p []byte) (int, error) {
	n, e := l.w.Write(p)
	if e == nil {
		l.w.(http.Flusher).Flush()
	}
	return n, e
}
func (l *recoveryHTTPLink) Close() error { return nil }
