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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// A lease carrying the operator's drain drains the relay to the lease's deadline (it stays up and keeps observing);
// once a lease no longer carries it, the process ends with ErrUndrained (a fence) so the restart serves again.
func TestObservationAppliesOperatorDrain(t *testing.T) {
	var mu sync.Mutex
	var draining bool
	var deadline int64
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			NodeID     string `json:"node_id"`
			Expected   uint64 `json:"expected_generation"`
			Generation uint64 `json:"node_generation"`
			Epoch      string `json:"process_epoch"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		gen := in.Generation
		if r.URL.Path == "/v1/relay/nodes/start" {
			gen = in.Expected + 1
		}
		mu.Lock()
		out := Lease{NodeID: in.NodeID, Generation: gen, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), Revocations: []Revocation{}, Draining: draining, DrainDeadline: deadline}
		mu.Unlock()
		json.NewEncoder(w).Encode(out)
	}))
	defer api.Close()
	c, err := New(Config{URL: api.URL, HTTP: api.Client(), Credential: strings.Repeat("test", 16), NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := c.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	server, err := derpquic.NewServer(derpquic.Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: lease.Generation, ProcessEpoch: lease.ProcessEpoch, Keys: map[string]ed25519.PublicKey{"test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Observe(ctx, server, false); err != nil || server.Snapshot().Draining {
		t.Fatalf("undrained lease: err %v draining %v", err, server.Snapshot().Draining)
	}
	mu.Lock()
	draining, deadline = true, time.Now().Add(10*time.Minute).Unix()
	mu.Unlock()
	for range 2 { // the drain repeats on every lease while it lasts and never fences
		if err = c.Observe(ctx, server, false); err != nil {
			t.Fatalf("drained lease: %v", err)
		}
		if !server.Snapshot().Draining || server.Ready() {
			t.Fatalf("drained lease left the relay draining=%v ready=%v", server.Snapshot().Draining, server.Ready())
		}
	}
	mu.Lock()
	draining, deadline = false, 0
	mu.Unlock()
	if err = c.Observe(ctx, server, false); !errors.Is(err, ErrUndrained) || !errors.Is(err, ErrFenced) {
		t.Fatalf("lifted drain: err %v, want ErrUndrained (a fence)", err)
	}
}
