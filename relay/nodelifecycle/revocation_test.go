package nodelifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// A revocation for an endpoint this relay does not hold, or the same one twice, is applied as a
// fence and never drains the relay; a malformed generation still fences.
func TestObservationAppliesUnknownAndRepeatedRevocations(t *testing.T) {
	var mu sync.Mutex
	revocations := []Revocation{{AccountID: "account_a", EndpointID: "endpoint_gone", MinGeneration: 3}, {AccountID: "account_a", EndpointID: "endpoint_gone", MinGeneration: 3}}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			NodeID     string `json:"node_id"`
			Expected   uint64 `json:"expected_generation"`
			Generation uint64 `json:"node_generation"`
			Epoch      string `json:"process_epoch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		out := Lease{NodeID: in.NodeID, Generation: in.Generation, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 256, Revocations: []Revocation{}}
		if r.URL.Path == "/v1/relay/nodes/start" {
			out.Generation = in.Expected + 1
		} else {
			mu.Lock()
			out.Revocations = revocations
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(out)
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
	if err := c.Observe(ctx, server, false); err != nil {
		t.Fatalf("unknown and repeated revocations: %v", err)
	}
	long := strings.Repeat("x", 257)
	for name, bad := range map[string]Revocation{
		"generation 1":      {AccountID: "account_a", EndpointID: "endpoint_gone", MinGeneration: 1},
		"generation 0":      {AccountID: "account_a", EndpointID: "endpoint_gone"},
		"empty ids":         {MinGeneration: 3},
		"empty account":     {EndpointID: "endpoint_gone", MinGeneration: 3},
		"empty endpoint":    {AccountID: "account_a", MinGeneration: 3},
		"oversize account":  {AccountID: long, EndpointID: "endpoint_gone", MinGeneration: 3},
		"oversize endpoint": {AccountID: "account_a", EndpointID: long, MinGeneration: 3},
	} {
		mu.Lock()
		// A valid entry listed before the malformed one must not turn the answer into a retry.
		revocations = []Revocation{{AccountID: "account_b", EndpointID: "endpoint_other", MinGeneration: 3}, bad}
		mu.Unlock()
		if err := c.Observe(ctx, server, false); err != ErrFenced {
			t.Fatalf("malformed revocation (%s) error = %v, want ErrFenced", name, err)
		}
	}
}
