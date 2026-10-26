package preview

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

type agentCarriersFake struct {
	mu      sync.Mutex
	hubs    []*DataCarrierPreviewHub
	changed chan struct{}
}

func (a *agentCarriersFake) Hubs() ([]*DataCarrierPreviewHub, <-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*DataCarrierPreviewHub(nil), a.hubs...), a.changed
}

func (a *agentCarriersFake) EndpointsServed(context.Context) bool { return true }

func (a *agentCarriersFake) set(hubs ...*DataCarrierPreviewHub) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hubs = hubs
	close(a.changed)
	a.changed = make(chan struct{})
}

// PLAN 3a: a preview is an endpoint route (route id = preview id) on every runtime carrier the
// agent holds. Readiness is reported once, after the server has seen an edge serve the route;
// losing one carrier keeps the preview on the other, and a replacement carrier serves it again.
func TestEndpointCarrierServesThePreviewOnEveryAgentCarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "served") }))
	defer origin.Close()
	identity := testPreviewCarrierIdentity(1)
	primary, secondary, replacement := newPreviewCarrierPair(t, ctx, identity), newPreviewCarrierPair(t, ctx, identity), newPreviewCarrierPair(t, ctx, identity)
	defer primary.close()
	defer secondary.close()
	defer replacement.close()
	hub := func(pair *previewCarrierPair) *DataCarrierPreviewHub {
		h, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	primaryHub, secondaryHub, replacementHub := hub(primary), hub(secondary), hub(replacement)
	defer replacementHub.Close()
	agent := &agentCarriersFake{hubs: []*DataCarrierPreviewHub{primaryHub, secondaryHub}, changed: make(chan struct{})}
	carrier, err := NewEndpointCarrier(agent, func(h *DataCarrierPreviewHub) (*DataCarrierPreviewCarrier, error) {
		return NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Hub: h, DialOrigin: func(ctx context.Context, target LeaseTarget) (io.ReadWriteCloser, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.Address)
		}})
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{ID: "preview_endpoint", AccountID: identity.AccountID, Generation: 1, AccessMode: "public", Endpoint: "https://bright-otter.example.test", Target: LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String()}, LeaseDeadline: time.Now().UTC().Add(time.Hour)}
	var reports atomic.Int32
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- carrier.Run(ctx, lease, func(Lease) error {
			// The first two reports find no edge serving the route yet.
			if reports.Add(1) <= 2 {
				return readinessFailed("POST", "http_status", http.StatusConflict, ErrDispatchNotServed, nil)
			}
			close(ready)
			return nil
		})
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("preview never reported ready")
	}
	get := func(pair *previewCarrierPair) string {
		t.Helper()
		stream, err := pair.edge.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if err := connectorprotocol.WriteStreamOpen(stream, testPreviewStreamOpen(identity, lease.ID, fmt.Sprintf("request_%d", time.Now().UnixNano()))); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(stream, "GET / HTTP/1.1\r\nHost: bright-otter.example.test\r\nConnection: close\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(stream), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body)
	}
	for _, pair := range []*previewCarrierPair{primary, secondary} {
		if got := get(pair); got != "served" {
			t.Fatalf("preview over a carrier = %q", got)
		}
	}
	// One carrier leaves: the other keeps serving. A replacement carrier serves the route again.
	_ = primaryHub.Close()
	agent.set(secondaryHub)
	if got := get(secondary); got != "served" {
		t.Fatalf("preview after losing one carrier = %q", got)
	}
	agent.set(secondaryHub, replacementHub)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if carrier.serving(replacementHub) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the route carrier probes the origin before accepting
	if got := get(replacement); got != "served" {
		t.Fatalf("preview over the replacement carrier = %q", got)
	}
	if reports.Load() != 3 {
		t.Fatalf("readiness reported %d times, want once after two not-served answers", reports.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint carrier did not stop")
	}
	_ = carrier.Close(context.Background())
	_ = secondaryHub.Close()
}

func (c *EndpointCarrier) serving(hub *DataCarrierPreviewHub) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.routes[hub] != nil
}

// Prod 2026-10-26 (1 in 4 previews never loaded): the route serves the edge's streams as soon as it
// is registered and its origin answers, while readiness is still being reported; a stream the
// edge opens during a slow report is answered, not left queued. Prior art: cloudflared and ngrok
// serve a tunnel from its registration on.
func TestPreviewRouteServesWhileReadinessIsStillReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "served") }))
	defer origin.Close()
	identity := testPreviewCarrierIdentity(1)
	pair := newPreviewCarrierPair(t, ctx, identity)
	defer pair.close()
	hub, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	agent := &agentCarriersFake{hubs: []*DataCarrierPreviewHub{hub}, changed: make(chan struct{})}
	carrier, err := NewEndpointCarrier(agent, func(h *DataCarrierPreviewHub) (*DataCarrierPreviewCarrier, error) {
		return NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Hub: h, DialOrigin: func(ctx context.Context, target LeaseTarget) (io.ReadWriteCloser, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.Address)
		}})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer carrier.Close(context.Background())
	lease := Lease{ID: "preview_slow_ready", AccountID: identity.AccountID, Generation: 1, AccessMode: "public", Endpoint: "https://slow-ready.example.test", Target: LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String()}, LeaseDeadline: time.Now().UTC().Add(time.Hour)}
	release := make(chan struct{})
	go func() {
		_ = carrier.Run(ctx, lease, func(Lease) error {
			// The server has not seen an edge serve the route yet, for longer than the test runs.
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer close(release)
	var body string
	for deadline := time.Now().Add(8 * time.Second); body != "served"; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("stream during a pending readiness report = %q, want the origin's answer", body)
		}
		body = func() string {
			streamCtx, done := context.WithTimeout(ctx, 2*time.Second)
			defer done()
			stream, err := pair.edge.OpenStream(streamCtx)
			if err != nil {
				return ""
			}
			defer stream.Close()
			stop := time.AfterFunc(2*time.Second, func() { _ = stream.Close() })
			defer stop.Stop()
			if err := connectorprotocol.WriteStreamOpen(stream, testPreviewStreamOpen(identity, lease.ID, fmt.Sprintf("request_%d", time.Now().UnixNano()))); err != nil {
				return ""
			}
			_, _ = io.WriteString(stream, "GET / HTTP/1.1\r\nHost: slow-ready.example.test\r\nConnection: close\r\n\r\n")
			response, err := http.ReadResponse(bufio.NewReader(stream), nil)
			if err != nil {
				return ""
			}
			defer response.Body.Close()
			got, _ := io.ReadAll(response.Body)
			return string(got)
		}()
	}
}
