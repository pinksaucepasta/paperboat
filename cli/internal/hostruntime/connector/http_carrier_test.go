package connector

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

func TestHTTPNetworkDialerUsesRealHTTP2AndHTTP3(t *testing.T) {
	for _, transport := range []Transport{HTTP2, HTTP3} {
		t.Run(string(transport), func(t *testing.T) {
			clientTLS, serverTLS, _ := testDataCarrierCertificates(t)
			identity := testDataCarrierIdentity()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					http.Error(w, "method", http.StatusMethodNotAllowed)
					return
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				buffer := make([]byte, 1)
				for {
					n, err := r.Body.Read(buffer)
					if n > 0 {
						_, _ = w.Write(buffer[:n])
						w.(http.Flusher).Flush()
					}
					if err != nil {
						return
					}
				}
			})
			var address string
			var closeServer func()
			if transport == HTTP2 {
				serverTLS.NextProtos = []string{"h2"}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server := &http.Server{Handler: handler, TLSConfig: serverTLS}
				if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
					t.Fatal(err)
				}
				go server.Serve(tls.NewListener(listener, serverTLS))
				address, closeServer = listener.Addr().String(), func() { _ = server.Close(); _ = listener.Close() }
			} else {
				serverTLS.NextProtos = []string{"h3"}
				packet, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server := &http3.Server{Handler: handler, TLSConfig: serverTLS, QUICConfig: &quic.Config{MaxIncomingStreams: 16, MaxIncomingUniStreams: 3}}
				go server.Serve(packet)
				address, closeServer = packet.LocalAddr().String(), func() { _ = server.Close(); _ = packet.Close() }
			}
			defer closeServer()
			endpoint := DataCarrierEndpointConfig{Address: address, TLS: clientTLS, ExpectedIdentity: identity, PeerBinding: func(state tls.ConnectionState) (DataCarrierIdentity, error) {
				if len(state.PeerCertificates) == 0 {
					return DataCarrierIdentity{}, errors.New("missing certificate")
				}
				return identity, nil
			}}
			config := NetworkDialerConfig{TCPMux: endpoint, QUIC: endpoint}
			dialCtx, cancelDial := context.WithCancel(ctx)
			result, err := NewHTTPNetworkDialer(config)(dialCtx, DataCarrierDialRequest{Transport: transport, Identity: identity})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			if result.Link == nil || result.Session != nil || result.PeerIdentity != identity {
				t.Fatalf("result = %+v", result)
			}
			cancelDial()
			if _, err := result.Link.Write([]byte{'x'}); err != nil {
				t.Fatalf("write after dial context cancellation: %v", err)
			}
			got := make([]byte, 1)
			if _, err := io.ReadFull(result.Link, got); err != nil || got[0] != 'x' {
				t.Fatalf("read after dial context cancellation = %q, %v", got, err)
			}
			if err := result.Link.Close(); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("close: %v", err)
			}
			wrong := identity
			wrong.HostID = "wrong-host"
			endpoint.PeerBinding = func(tls.ConnectionState) (DataCarrierIdentity, error) { return wrong, nil }
			_, err = NewHTTPNetworkDialer(NetworkDialerConfig{TCPMux: endpoint, QUIC: endpoint})(ctx, DataCarrierDialRequest{Transport: transport, Identity: identity})
			var dialError *TransportDialError
			if !errors.Is(err, ErrDataCarrierTLS) || !errors.As(err, &dialError) || dialError.Fallback {
				t.Fatalf("wrong authenticated peer must fail without fallback: %v", err)
			}
		})
	}
}

func TestHTTPNetworkDialerCancellationDuringHandshake(t *testing.T) {
	clientTLS, _, _ := testDataCarrierCertificates(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	endpoint := DataCarrierEndpointConfig{Address: listener.Addr().String(), TLS: clientTLS, PeerBinding: func(tls.ConnectionState) (DataCarrierIdentity, error) { return testDataCarrierIdentity(), nil }}
	_, err = NewHTTPNetworkDialer(NetworkDialerConfig{TCPMux: endpoint})(ctx, DataCarrierDialRequest{Transport: HTTP2})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handshake cancellation = %v", err)
	}
	select {
	case connection := <-accepted:
		_ = connection.Close()
	default:
	}
}
