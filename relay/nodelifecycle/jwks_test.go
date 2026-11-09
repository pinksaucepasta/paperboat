package nodelifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// A signing-key rotation reaches a running relay with its next observation, no restart or re-enrollment:
// the new key is trusted next to the old one, a withdrawal drops the old one, a malformed document changes nothing.
func TestObservationAppliesIssuerTrustDocument(t *testing.T) {
	var mu sync.Mutex
	var document string
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
		out := Lease{NodeID: in.NodeID, Generation: gen, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 256, Revocations: []Revocation{}}
		if document != "" {
			out.JWKS = json.RawMessage(document)
		}
		mu.Unlock()
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
	pub := func() ed25519.PublicKey { p, _, _ := ed25519.GenerateKey(rand.Reader); return p }
	startup := map[string]ed25519.PublicKey{"old": pub()}
	server, err := derpquic.NewServer(derpquic.Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: lease.Generation, ProcessEpoch: lease.ProcessEpoch, Keys: startup})
	if err != nil {
		t.Fatal(err)
	}
	doc := func(withdrawn string, ids ...string) string {
		keys := ""
		for _, id := range ids {
			keys += `,{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"` + id + `","x":"` + base64.RawURLEncoding.EncodeToString(pub()) + `"}`
		}
		return `{"keys":[` + keys[1:] + `]` + withdrawn + `}`
	}
	serve := func(d string) {
		mu.Lock()
		document = d
		mu.Unlock()
	}
	observe := func() error { return c.Observe(ctx, server, false) }
	if err = observe(); err != nil || !slices.Equal(server.KeyIDs(), []string{"old"}) {
		t.Fatalf("an observation without a trust document changed the keys: %v %v", server.KeyIDs(), err)
	}
	serve(doc("", "old", "new"))
	if err = observe(); err != nil || !slices.Equal(server.KeyIDs(), []string{"new", "old"}) {
		t.Fatalf("rotation not applied after one observation: %v %v", server.KeyIDs(), err)
	}
	serve(doc(`,"withdrawn_key_ids":["old"]`, "old", "new"))
	if err = observe(); err != nil || !slices.Equal(server.KeyIDs(), []string{"new"}) {
		t.Fatalf("withdrawal not applied: %v %v", server.KeyIDs(), err)
	}
	serve(`{"keys":[]}`)
	if err = observe(); !errors.Is(err, ErrControl) || !slices.Equal(server.KeyIDs(), []string{"new"}) {
		t.Fatalf("a malformed document replaced the keys: %v %v", server.KeyIDs(), err)
	}
}
