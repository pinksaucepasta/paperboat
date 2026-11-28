package edgehttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startingDir is a forwarder whose holder state has not loaded until seed is closed.
type startingDir struct {
	seed      chan struct{}
	holds     atomic.Bool // after seeding, the name is held elsewhere
	forwarded atomic.Int64
}

func (d *startingDir) Starting() bool {
	select {
	case <-d.seed:
		return false
	default:
		return true
	}
}
func (d *startingDir) AwaitSeeded(ctx context.Context) bool {
	select {
	case <-d.seed:
		return true
	case <-ctx.Done():
		return false
	}
}
func (d *startingDir) Holders(string) []string {
	if !d.Starting() && d.holds.Load() {
		return []string{"peer"}
	}
	return nil
}
func (d *startingDir) Forward(_ context.Context, conn net.Conn, _ string) error {
	d.forwarded.Add(1)
	return conn.Close()
}

// A fresh edge that has not loaded who holds which name never answers a definitive 404 for a name it does not
// serve: it waits briefly for the first load, forwards if a holder then appears, and otherwise answers a retryable
// 503. Once loaded, an unknown name is a 404 again.
func TestSharedTLSStartingEdgeNeverAnswersNotFound(t *testing.T) {
	match, _ := browserTestMatch()
	clientTLS, serverTLS, _, _, _ := task24Certificates(t, match.Host)
	clientTLS.ServerName = match.Host
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{})
	defer registry.Close()
	shared, err := NewSharedTLSListener(raw, &takedownAuthority{}, registry, func(string) bool { return false }, "")
	if err != nil {
		t.Fatal(err)
	}
	shared.ServeUnknownNamesWithPage(serverTLS)
	dir := &startingDir{seed: make(chan struct{})}
	shared.SetForwarding(ForwardHooks{Forwarder: dir})
	server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	defer server.Close()
	go server.ServeTLS(shared, "", "")
	get := func() (*http.Response, string, error) {
		client := &http.Client{Timeout: exchangeBudget + startupSeedWait, Transport: &http.Transport{TLSClientConfig: clientTLS, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", raw.Addr().String())
		}}}
		response, err := client.Get("https://" + match.Host + "/")
		if err != nil {
			return nil, "", err
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return response, string(body), nil
	}

	response, body, err := get()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") == "" || response.Header.Get("X-Paperboat-Error") != "edge_starting" {
		t.Fatalf("before the first load = %d %v %q, want a retryable 503", response.StatusCode, response.Header, body)
	}

	// The first load lands while the visitor waits, naming a holder: the visitor is forwarded.
	dir.holds.Store(true)
	go func() { time.Sleep(300 * time.Millisecond); close(dir.seed) }()
	_, _, _ = get() // the fake holder closes the connection
	if dir.forwarded.Load() != 1 {
		t.Fatalf("forwarded %d times, want 1 once the holder loaded", dir.forwarded.Load())
	}

	dir.holds.Store(false)
	response, body, err = get()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || !strings.Contains(body, notFoundMessage) {
		t.Fatalf("unknown name after the load = %d %q, want 404", response.StatusCode, body)
	}
}
