package derpquic

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

func (f *fixture) wss(server *Server, cert tls.Certificate, g Grant, proxy func(*http.Request) (*url.URL, error)) (*WSSClient, string) {
	f.t.Helper()
	h := httptest.NewUnstartedServer(server.WSSHandler())
	h.TLS = &tls.Config{Certificates: f.tls.Certificates, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12}
	h.StartTLS()
	f.t.Cleanup(h.Close)
	c := NewWSSClient(WSSClientConfig{URL: "wss" + strings.TrimPrefix(h.URL, "https"), TLS: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}}, Credential: func(context.Context) (string, error) { return f.token(g), nil }, Proxy: proxy})
	f.t.Cleanup(func() { _ = c.Close() })
	return c, h.Listener.Addr().String()
}

func connectWSS(t *testing.T, c *WSSClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
}

func receiveWSS(t *testing.T, c *WSSClient, want key.NodePublic, payload []byte) {
	t.Helper()
	done := make(chan struct {
		m   derp.ReceivedMessage
		err error
	}, 1)
	go func() {
		m, _, err := c.RecvDetail()
		done <- struct {
			m   derp.ReceivedMessage
			err error
		}{m, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		p, ok := got.m.(derp.ReceivedPacket)
		if !ok || p.Source != want || !bytes.Equal(p.Data, payload) {
			t.Fatal("forwarded source or bytes differ")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("receive timed out")
	}
}

func connectProxy(t *testing.T) (*url.URL, *atomic.Int64) {
	t.Helper()
	var connects atomic.Int64
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, "unreachable", http.StatusBadGateway)
			return
		}
		h, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "unsupported", http.StatusInternalServerError)
			return
		}
		client, rw, err := h.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		connects.Add(1)
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
		go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
	}))
	t.Cleanup(p.Close)
	u, err := url.Parse(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u, &connects
}

func TestMixedQUICWSSViaProxyAndRevocation(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	pairScopes(&ga, &gb)
	a := f.client(address, ca, ga)
	proxyURL, connects := connectProxy(t)
	b, _ := f.wss(s, cb, gb, func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	connect(t, a)
	connectWSS(t, b)
	if connects.Load() != 1 {
		t.Fatalf("WSS did not use CONNECT proxy: %d", connects.Load())
	}
	payload := bytes.Repeat([]byte{0xa5}, 1312)
	if err := a.Send(kb, payload); err != nil {
		t.Fatal(err)
	}
	receiveWSS(t, b, ka, payload)
	if err := b.Send(ka, payload); err != nil {
		t.Fatal(err)
	}
	receive(t, a, kb, payload)
	if got := s.Snapshot(); got.Connections != 2 || got.Forwarded != 2 {
		t.Fatalf("mixed accounting: %+v", got)
	}
	if err := s.Revoke("account", "b", 2); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.current != nil && b.current.ctx.Err() != nil })
	if err := b.Connect(context.Background()); err != ErrAdmission {
		t.Fatalf("revoked WSS admission: %v", err)
	}
}

func TestWSSAccountConnectionBound(t *testing.T) {
	f := newFixture(t)
	s, _, _ := f.start("127.0.0.1:0")
	var clients []*WSSClient
	for i := 0; i < MaxAccountConnections; i++ {
		k, cert := key.NewNode().Public(), certificate(t)
		g := f.grant(k, cert, string(rune('a'+i)))
		c, _ := f.wss(s, cert, g, nil)
		connectWSS(t, c)
		clients = append(clients, c)
	}
	k, cert := key.NewNode().Public(), certificate(t)
	g := f.grant(k, cert, "overflow")
	overflow, _ := f.wss(s, cert, g, nil)
	if err := overflow.Connect(context.Background()); err != ErrOverload {
		t.Fatalf("WSS account overload: %v", err)
	}
	eventually(t, func() bool { return s.Snapshot().Connections == MaxAccountConnections })
}

func TestRealWSSFallsBackThenRecoversToQUIC(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	k, cert := key.NewNode().Public(), certificate(t)
	g := f.grant(k, cert, "recover")
	wss, _ := f.wss(s, cert, g, nil)
	reserved, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.LocalAddr().String()
	_ = reserved.Close()
	credential := func(context.Context) (string, error) { return f.token(g), nil }
	quic := NewClient(ClientConfig{Address: address, TLS: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}}, Credential: credential})
	carrier := NewFallbackCarrier(quic, wss, 10*time.Millisecond)
	defer carrier.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if err = carrier.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if active, _ := carrier.current(); active != wss {
		t.Fatal("unreachable UDP did not select WSS")
	}
	socket, err := net.ListenPacket("udp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	serveCtx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	go func() { done <- s.Serve(serveCtx, socket, f.tls) }()
	eventually(t, func() bool { active, _ := carrier.current(); return active == quic })
	eventually(t, func() bool { got := s.Snapshot(); return got.Accepted >= 2 && got.Connections == 1 })
	stop()
	_ = s.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("QUIC recovery server did not stop")
	}
	s.Wait()
}

func TestWSSExpiredLeaseRecoversWithFreshAuthority(t *testing.T) { testWSSExpiredLease(t, false) }
func TestWSSRefreshCredentialExpiryRecovers(t *testing.T)        { testWSSExpiredLease(t, true) }
func testWSSExpiredLease(t *testing.T, callback bool) {
	f := newFixture(t)
	server, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	pairScopes(&ga, &gb)
	if !callback {
		ga.ExpiresAt = time.Now().Add(2 * time.Second).Unix()
	}
	a, _ := f.wss(server, ca, ga, nil)
	var mu sync.Mutex
	token := f.token(ga)
	callbackExpired := false
	a.config.Credential = func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if callbackExpired {
			return "", ErrExpired
		}
		return token, nil
	}
	b := f.client(address, cb, gb)
	connectWSS(t, a)
	connect(t, b)
	if callback {
		mu.Lock()
		callbackExpired = true
		mu.Unlock()
	}
	a.mu.Lock()
	old := a.current
	a.mu.Unlock()
	select {
	case <-old.ctx.Done():
	case <-time.After(18 * time.Second):
		t.Fatal("expired WSS lease remained connected")
	}
	if err := a.Connect(t.Context()); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired WSS grant: %v", err)
	}
	ga.Generation++
	ga.IssuedAt = time.Now().Unix()
	ga.ExpiresAt = ga.IssuedAt + 30
	mu.Lock()
	token = f.token(ga)
	callbackExpired = false
	mu.Unlock()
	if err := a.Connect(t.Context()); err != nil {
		t.Fatalf("fresh authority did not recover WSS: %v", err)
	}
	payload := []byte("renewed WSS")
	if err := a.Send(kb, payload); err != nil {
		t.Fatal(err)
	}
	receive(t, b, ka, payload)
}
