package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestE2EProxy is the proxy of the multi-instance CI stage (tools/e2e/run.sh): TLS in front of
// PAPERBOAT_E2E_BACKENDS (comma separated host:port), round-robin over the healthy ones. A backend
// whose connection is refused is dropped at once and the request goes to the next (nothing was sent),
// and a probe of /healthz every 500 ms brings it back. Long-lived connections to a killed instance
// break; their clients reconnect through the proxy. Prior art: Envoy and Caddy upstream health checks.
func TestE2EProxy(t *testing.T) {
	directory, backends := os.Getenv("PAPERBOAT_E2E_DIR"), strings.Split(os.Getenv("PAPERBOAT_E2E_BACKENDS"), ",")
	if directory == "" || backends[0] == "" {
		t.Skip("CI end-to-end proxy only")
	}
	p := &roundRobin{backends: backends, down: make([]atomic.Bool, len(backends)), transport: http.DefaultTransport.(*http.Transport).Clone()}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go p.probe(ctx)
	rewrite := func(r *httputil.ProxyRequest) { r.Out.URL.Scheme, r.Out.URL.Host = "http", "backend" } // RoundTrip picks the host
	server := &http.Server{Addr: os.Getenv("PAPERBOAT_E2E_LISTEN"), ReadHeaderTimeout: 5 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		Handler: logRequests(t, &httputil.ReverseProxy{Rewrite: rewrite, Transport: p, FlushInterval: -1})}
	go func() {
		_ = server.ListenAndServeTLS(os.Getenv("PAPERBOAT_E2E_TLS_CERT"), os.Getenv("PAPERBOAT_E2E_TLS_KEY"))
	}()
	defer server.Close()
	for tick := time.NewTicker(time.Second); ; <-tick.C {
		if _, err := os.Stat(filepath.Join(directory, "stop")); err == nil {
			return
		}
	}
}

type roundRobin struct {
	backends  []string
	down      []atomic.Bool
	next      atomic.Uint32
	transport *http.Transport
}

func (p *roundRobin) RoundTrip(r *http.Request) (*http.Response, error) {
	start := int(p.next.Add(1))
	for i := range p.backends {
		n := (start + i) % len(p.backends)
		if p.down[n].Load() {
			continue
		}
		out := r.Clone(r.Context())
		out.URL.Host = p.backends[n]
		response, err := p.transport.RoundTrip(out)
		var opError *net.OpError
		if errors.As(err, &opError) && opError.Op == "dial" {
			p.down[n].Store(true)
			continue
		}
		return response, err
	}
	return nil, errors.New("no healthy backend")
}

func (p *roundRobin) probe(ctx context.Context) {
	for tick := time.NewTicker(500 * time.Millisecond); ; {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for n, backend := range p.backends {
			client := http.Client{Timeout: 2 * time.Second}
			response, err := client.Get("http://" + backend + "/healthz")
			if err == nil {
				response.Body.Close()
			}
			p.down[n].Store(err != nil || response.StatusCode != http.StatusOK)
		}
	}
}
