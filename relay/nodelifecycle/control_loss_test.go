package nodelifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// F21-01: with the control plane slow past the 15 s lease (after an API cut on staging) the relay drained, exited
// and restarted under a new epoch every ~25 s, ending every relayed session. It now keeps serving while control does
// not answer, and only an explicit refusal fences it.
func TestRelayKeepsServingWhileControlDoesNotAnswer(t *testing.T) {
	const credential = "testtesttesttesttesttesttesttesttesttesttesttesttesttesttesttest"
	var mode atomic.Int32 // 0 answer, 1 hang, 2 refuse
	release := make(chan struct{})
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			NodeID             string `json:"node_id"`
			ExpectedGeneration uint64 `json:"expected_generation"`
			Generation         uint64 `json:"node_generation"`
			Epoch              string `json:"process_epoch"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		gen := in.Generation
		if r.URL.Path == "/v1/relay/nodes/start" {
			gen = in.ExpectedGeneration + 1
		} else if mode.Load() == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		} else if mode.Load() == 2 {
			http.Error(w, "fenced", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(Lease{NodeID: in.NodeID, Generation: gen, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(2 * time.Second).Unix(), CapacityLimit: 256, Revocations: []Revocation{}})
	}))
	defer api.Close()
	defer close(release)
	c, err := New(Config{URL: api.URL, HTTP: api.Client(), Credential: credential, NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	lease, err := c.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	server, err := derpquic.NewServer(derpquic.Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: lease.Generation, ProcessEpoch: lease.ProcessEpoch, Keys: map[string]ed25519.PublicKey{"test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	mode.Store(1) // the control plane stops answering: the 2 s lease runs out
	go func() { done <- c.Run(ctx, server) }()
	select {
	case err := <-done:
		t.Fatalf("the relay stopped %v after its lease ran out without an answer: %v", time.Since(time.Unix(lease.ExpiresAt, 0)), err)
	case <-time.After(8 * time.Second):
	}
	if server.Snapshot().Draining {
		t.Fatal("the relay drains while the control plane does not answer")
	}
	mode.Store(2) // an explicit refusal still fences it
	select {
	case err := <-done:
		if !errors.Is(err, ErrFenced) || !server.Snapshot().Draining {
			t.Fatalf("an explicit refusal: err %v, draining %v; want fenced and draining", err, server.Snapshot().Draining)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("an explicit refusal did not fence the relay")
	}
}
