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

// A lease's pool_handover list replaces the relay's list, an absent field keeps it (the control plane could not
// read it), and an empty list clears it.
func TestObservationAppliesPoolHandoverList(t *testing.T) {
	var mu sync.Mutex
	var handover *[]PoolHandover
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
		out := Lease{NodeID: in.NodeID, Generation: gen, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), Revocations: []Revocation{}, PoolHandover: handover}
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
	set := func(list *[]PoolHandover) {
		mu.Lock()
		handover = list
		mu.Unlock()
		if err := c.Observe(ctx, server, false); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(time.Hour).Unix()
	set(&[]PoolHandover{{AccountID: "b", CloseAt: at}, {AccountID: "a", CloseAt: at}})
	if got := server.PoolHandover(); len(got) != 2 || got[0].AccountID != "a" || got[1].AccountID != "b" || got[0].CloseAt.Unix() != at {
		t.Fatalf("applied %v", got)
	}
	set(nil)
	if len(server.PoolHandover()) != 2 {
		t.Fatalf("an absent list changed the relay's list: %v", server.PoolHandover())
	}
	set(&[]PoolHandover{})
	if len(server.PoolHandover()) != 0 {
		t.Fatalf("an empty list did not clear: %v", server.PoolHandover())
	}
}
