package runtime

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPrivateHTTPServerLifecycle(t *testing.T) {
	server, err := NewHTTPServer(HTTPServerSpec{Address: "127.0.0.1:19000", Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	listener := newBlockingListener()
	server.listen = func(string, string) (net.Listener, error) { return listener, nil }
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateHTTPServerRejectsPublicAndUnboundedConfiguration(t *testing.T) {
	tests := []HTTPServerSpec{{Address: "0.0.0.0:19000", Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096}, {Address: "127.0.0.1:0", Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096}, {Address: "127.0.0.1:19000", Handler: http.NotFoundHandler(), MaxHeaderBytes: 4096}, {Address: "127.0.0.1:19000", Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 0}}
	for _, spec := range tests {
		if _, err := NewHTTPServer(spec); err == nil {
			t.Fatalf("unsafe server accepted: %+v", spec)
		}
	}
}

func TestPublicRedirectServerAcceptsCleartextWildcardListener(t *testing.T) {
	reserved, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(reserved.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = reserved.Close()
	server, err := NewHTTPServer(HTTPServerSpec{Address: net.JoinHostPort("0.0.0.0", port), Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096, Role: HTTPServerPublicRedirect})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPublicRolesRequireMatchingTransport(t *testing.T) {
	base := HTTPServerSpec{Address: "0.0.0.0:19000", Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096}
	publicTLS := base
	publicTLS.Role = HTTPServerPublicTLS
	redirectTLS := base
	redirectTLS.Role = HTTPServerPublicRedirect
	redirectTLS.TLSConfig = &tls.Config{}
	for _, spec := range []HTTPServerSpec{base, publicTLS, redirectTLS} {
		if _, err := NewHTTPServer(spec); err == nil {
			t.Fatalf("mismatched public server accepted: %+v", spec)
		}
	}
}

func TestHTTPServerWrapperFailureClosesOwnedSocket(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewHTTPServer(HTTPServerSpec{Address: listener.Addr().String(), Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096, WrapListener: func(net.Listener) (net.Listener, error) { return nil, ErrHTTPServerInvalid }})
	if err != nil {
		t.Fatal(err)
	}
	server.listen = func(string, string) (net.Listener, error) { return listener, nil }
	if err = server.Start(t.Context()); err == nil {
		t.Fatal("wrapper failure accepted")
	}
	if _, err = listener.Accept(); err == nil {
		t.Fatal("socket retained after startup failure")
	}
}

func TestHTTPServerEnforcesConfiguredHeaderLimit(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()
	server, err := NewHTTPServer(HTTPServerSpec{Address: address, Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("GET / HTTP/1.1\r\nHost: example.test\r\nX-Large: " + strings.Repeat("x", 8<<10) + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d", response.StatusCode)
	}
}
