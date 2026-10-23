package nodelifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Each observation (~3 s) carried every admitted endpoint as a subject: ~400 KB at 4,096 connections.
// A steady node sends the list once and then only its digest; the control plane asks for it again with
// subjects_required, and such an answer keeps the node's last quota list.
func TestObservationSendsSubjectsOnlyWhenTheSetChanged(t *testing.T) {
	credential := strings.Repeat("test", 16)
	var mu sync.Mutex
	var full []bool
	var digests []string
	require := false
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			NodeID     string                       `json:"node_id"`
			Expected   uint64                       `json:"expected_generation"`
			Generation uint64                       `json:"node_generation"`
			Epoch      string                       `json:"process_epoch"`
			Digest     string                       `json:"subjects_digest"`
			Subjects   *[]derpquic.AuthoritySubject `json:"subjects"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		gen := in.Generation
		out := Lease{NodeID: in.NodeID, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 256, Revocations: []Revocation{}}
		mu.Lock()
		if r.URL.Path == "/v1/relay/nodes/start" {
			gen = in.Expected + 1
		} else {
			full = append(full, in.Subjects != nil)
			digests = append(digests, in.Digest)
			if require {
				out.SubjectsRequired, require = true, false
			} else {
				out.QuotaDenied = []string{"account_q"}
			}
		}
		mu.Unlock()
		out.Generation = gen
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
	observe := func() {
		t.Helper()
		if err := c.Observe(ctx, server, false); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		observe()
	}
	mu.Lock()
	require = true
	mu.Unlock()
	observe() // answered with subjects_required
	if got := server.QuotaDenied(); !slices.Equal(got, []string{"account_q"}) {
		t.Fatalf("subjects_required answer replaced the quota list with %v", got)
	}
	observe() // resends the list in full
	observe()
	mu.Lock()
	defer mu.Unlock()
	if want := []bool{true, false, false, false, true, false}; !slices.Equal(full, want) {
		t.Fatalf("full subjects sent on observations %v, want %v", full, want)
	}
	empty := sha256.Sum256(nil)
	for _, digest := range digests {
		if digest != hex.EncodeToString(empty[:]) {
			t.Fatalf("digest %q for an empty set, want sha256 of nothing", digest)
		}
	}
}

// The digest is a wire contract: the control plane's vector (relay_subjects_test.go) for the
// sorted membership lines printf 'acct_a\0ep_b\n' 'acct_a\0ep_z\n' 'acct_b\0ep_a\n'.
func TestSubjectsDigestIsTheSortedMembershipLinesHash(t *testing.T) {
	a := []derpquic.AuthoritySubject{{AccountID: "acct_b", EndpointID: "ep_a"}, {AccountID: "acct_a", EndpointID: "ep_z"}, {AccountID: "acct_a", EndpointID: "ep_b"}}
	b := []derpquic.AuthoritySubject{a[2], a[0], a[1]}
	if subjectsDigest(a) != subjectsDigest(b) {
		t.Fatal("digest depends on order")
	}
	if got := subjectsDigest(a); got != "2c9bbe3e9c2b06d2aca7a694fa3c83370dc4dc7145c822e27c318e3e90f7607d" {
		t.Fatalf("digest %s does not match the control plane's vector", got)
	}
}
