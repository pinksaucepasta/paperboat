package httptransport

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stallingProxy forwards TCP to target. stallExisting makes every connection open at that moment
// swallow bytes in both directions without closing, the way a flow looks after a network outage
// whose retransmission timer has backed off: the socket stays up, nothing gets through.
// Connections accepted afterwards forward normally.
type stallingProxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	open     []*atomic.Bool
}

func newStallingProxy(t *testing.T, target string) *stallingProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &stallingProxy{listener: listener, target: target}
	t.Cleanup(func() { _ = listener.Close() })
	go proxy.serve()
	return proxy
}

func (p *stallingProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		stalled := new(atomic.Bool)
		p.mu.Lock()
		p.open = append(p.open, stalled)
		p.mu.Unlock()
		go pipe(server, client, stalled)
		go pipe(client, server, stalled)
	}
}

func pipe(dst, src net.Conn, stalled *atomic.Bool) {
	defer dst.Close()
	defer src.Close()
	buffer := make([]byte, 32<<10)
	for {
		n, err := src.Read(buffer)
		if n > 0 && !stalled.Load() {
			if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *stallingProxy) stallExisting() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, stalled := range p.open {
		stalled.Store(true)
	}
}

func TestTransportReplacesAConnectionThatStalledDuringAnOutage(t *testing.T) {
	for name, mark := range map[string]func(context.Context) context.Context{
		"request":   func(ctx context.Context) context.Context { return ctx },
		"long poll": LongPoll,
	} {
		t.Run(name, func(t *testing.T) { replacesStalledConnection(t, mark) })
	}
}

func replacesStalledConnection(t *testing.T, mark func(context.Context) context.Context) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	proxy := newStallingProxy(t, server.Listener.Addr().String())

	config := DevelopmentConfig()
	config.ProxySource = StaticProxySource{}
	config.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} // test server certificate
	config.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, proxy.listener.Addr().String())
	}
	config.HealthCheckInterval = 200 * time.Millisecond
	config.HealthCheckTimeout = 200 * time.Millisecond
	transport, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	get := func(timeout time.Duration) error {
		ctx, cancel := context.WithTimeout(mark(t.Context()), timeout)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.ProtoMajor != 2 {
			t.Fatalf("protocol = %s, want HTTP/2 (the pooled, multiplexed connection is what stalls)", response.Proto)
		}
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}

	if err := get(5 * time.Second); err != nil {
		t.Fatalf("request before the outage: %v", err)
	}
	proxy.stallExisting()
	if err := get(300 * time.Millisecond); err == nil {
		t.Fatal("a request on the stalled connection succeeded; the proxy did not stall it")
	}

	// The network is back for new connections. The stalled one must be noticed and replaced within
	// one health check, not ridden by every request until its retransmission timer gives up.
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := get(300 * time.Millisecond)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still failing 3 s after the network came back: %v", err)
		}
	}
}
