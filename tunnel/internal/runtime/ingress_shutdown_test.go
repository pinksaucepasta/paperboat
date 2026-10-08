package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestIngressShutdownAfterWatcherConsumedCompletion(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server, err := NewHTTPServer(HTTPServerSpec{Address: listener.Addr().String(), Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096})
		if err != nil {
			listener.Close()
			t.Fatal(err)
		}
		server.listen = func(string, string) (net.Listener, error) { return listener, nil }
		if err := server.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		_ = server.server.Close()
		select {
		case <-server.Done():
		case <-time.After(time.Second):
			t.Fatal("HTTP completion not signaled")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Fatal("watcher consumption prevented HTTP cleanup", err)
		}
	})
	t.Run("http3", func(t *testing.T) {
		tlsConfig := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return nil, errors.New("fixture accepts no TLS clients")
		}}
		server, err := NewHTTP3Server("127.0.0.1:0", http.NotFoundHandler(), tlsConfig, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		_ = server.server.Close()
		_ = server.packet.Close()
		select {
		case <-server.Done():
		case <-time.After(time.Second):
			t.Fatal("HTTP3 completion not signaled")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Fatal("watcher consumption prevented HTTP3 cleanup", err)
		}
	})
}
