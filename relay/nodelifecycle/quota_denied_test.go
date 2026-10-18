package nodelifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
)

// F13-03 relay admission: the observation announces that this relay understands the denial
// list (older control planes and relays never see the field they cannot decode), and applies
// exactly what the control plane lists, replacing the previous list every time.
func TestObservationAppliesQuotaDeniedListAndAnnouncesSupport(t *testing.T) {
	credential := strings.Repeat("test", 16)
	var mu sync.Mutex
	var denied []string
	var announced bool
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			NodeID       string `json:"node_id"`
			Expected     uint64 `json:"expected_generation"`
			Generation   uint64 `json:"node_generation"`
			Epoch        string `json:"process_epoch"`
			QuotaSupport bool   `json:"quota_denied_supported"`
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
		announced = announced || in.QuotaSupport
		out := Lease{NodeID: in.NodeID, Generation: gen, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 256, Revocations: []derpquic.AuthoritySubject{}, QuotaDenied: denied}
		mu.Unlock()
		json.NewEncoder(w).Encode(out)
	}))
	defer api.Close()
	c, err := New(Config{URL: api.URL, HTTP: api.Client(), Credential: credential, NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state")})
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
	set := func(accounts ...string) {
		mu.Lock()
		denied = accounts
		mu.Unlock()
	}
	set("account_b", "account_a")
	if err := c.Observe(ctx, server, false); err != nil {
		t.Fatal(err)
	}
	if got := server.QuotaDenied(); !slices.Equal(got, []string{"account_a", "account_b"}) {
		t.Fatalf("applied %v, want both listed accounts", got)
	}
	set("account_b")
	if err := c.Observe(ctx, server, false); err != nil || !slices.Equal(server.QuotaDenied(), []string{"account_b"}) {
		t.Fatalf("a shorter list did not replace the previous one: %v %v", server.QuotaDenied(), err)
	}
	set()
	if err := c.Observe(ctx, server, false); err != nil || len(server.QuotaDenied()) != 0 {
		t.Fatalf("an empty list did not clear refusals: %v %v", server.QuotaDenied(), err)
	}
	mu.Lock()
	supported := announced
	mu.Unlock()
	if !supported {
		t.Fatal("the observation did not announce quota_denied_supported")
	}
}
