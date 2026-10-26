// Package eventstest is a stand-in control plane for tests of machine event consumers: it holds
// waits open like the real endpoint and answers them when a topic is published.
package eventstest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
)

// Hold is how long the stand-in keeps a wait open (the real endpoint holds 30 s).
const Hold = 150 * time.Millisecond

// Proof records one signed request.
type Proof struct{ OperationID, Method, Path string }

type auth struct{ proofs chan Proof }

func (auth) Token(context.Context) (string, error) { return "identity-token", nil }
func (a auth) Proof(_ context.Context, operationID, method, path string, _ []byte) ([]byte, error) {
	select {
	case a.proofs <- Proof{operationID, method, path}:
	default:
	}
	return []byte("proof-bytes"), nil
}

// Plane answers waits from topic generations the test publishes.
type Plane struct {
	mu          sync.Mutex
	generations map[string]int64
	wake        chan struct{}
	requests    []map[string]int64
	headers     []http.Header
	failures    atomic.Int32
	failStatus  atomic.Int32
	proofs      chan Proof
}

// Publish bumps a topic's generation and releases waiting requests.
func (p *Plane) Publish(topic machineevents.Topic) {
	p.mu.Lock()
	p.generations[string(topic)]++
	p.mu.Unlock()
	p.wake <- struct{}{}
}

// FailNext makes the next n requests answer 503, including a wait that is being held now.
func (p *Plane) FailNext(n int32) { p.FailNextWith(n, http.StatusServiceUnavailable) }

// FailNextWith answers the next n waits with status (401: this machine was revoked).
func (p *Plane) FailNextWith(n int32, status int) {
	p.failStatus.Store(int32(status))
	p.failures.Store(n)
}

// PendingFailures is how many scripted failures have not been served yet.
func (p *Plane) PendingFailures() int32 { return p.failures.Load() }

// Requests returns the generations each request carried, in order.
func (p *Plane) Requests() []map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]int64(nil), p.requests...)
}

// Headers returns the headers of request i.
func (p *Plane) Headers(i int) http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.headers[i].Clone()
}

// NextProof returns the next recorded signature input.
func (p *Plane) NextProof() Proof { return <-p.proofs }

func (p *Plane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Topics map[string]int64 `json:"topics"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || r.URL.Path != machineevents.Path || r.Method != http.MethodPost {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.requests = append(p.requests, request.Topics)
	p.headers = append(p.headers, r.Header.Clone())
	p.mu.Unlock()
	for {
		// A scripted failure also ends a wait that is already held, so a test that calls FailNext and
		// then publishes gets the outage it asked for whichever request was in flight.
		if p.failures.Load() > 0 {
			p.failures.Add(-1)
			http.Error(w, "refused", int(p.failStatus.Load()))
			return
		}
		p.mu.Lock()
		moved := map[string]int64{}
		for topic, known := range request.Topics {
			if p.generations[topic] > known {
				moved[topic] = p.generations[topic]
			}
		}
		p.mu.Unlock()
		if len(moved) > 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"topics": moved})
			return
		}
		select {
		case <-p.wake:
		case <-r.Context().Done():
			return
		case <-time.After(Hold):
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
}

// Start runs a Hub against a new Plane, with a subscription per topic. The hub stops with the test.
func Start(t *testing.T, fallback time.Duration, topics ...machineevents.Topic) (*machineevents.Hub, []*machineevents.Subscription, *Plane) {
	t.Helper()
	return StartWith(t, fallback, func(server *httptest.Server) http.RoundTripper { return server.Client().Transport }, topics...)
}

// StartWith is Start with the hub's transport built for the stand-in server, for tests of the
// production transport against a held wait.
func StartWith(t *testing.T, fallback time.Duration, transport func(*httptest.Server) http.RoundTripper, topics ...machineevents.Topic) (*machineevents.Hub, []*machineevents.Subscription, *Plane) {
	t.Helper()
	plane := &Plane{generations: map[string]int64{}, wake: make(chan struct{}, 64), proofs: make(chan Proof, 64)}
	server := httptest.NewTLSServer(plane)
	t.Cleanup(server.Close)
	signer := auth{proofs: plane.proofs}
	var operations atomic.Int64
	hub, err := machineevents.New(machineevents.Config{
		ControlURL: server.URL, Tokens: signer, Proofs: signer, Transport: transport(server), FallbackInterval: fallback,
		OperationID: func() (string, error) { return fmt.Sprintf("operation_%08d", operations.Add(1)), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriptions := make([]*machineevents.Subscription, len(topics))
	for i, topic := range topics {
		subscriptions[i] = hub.Subscribe(topic)
	}
	if err := hub.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })
	return hub, subscriptions, plane
}
