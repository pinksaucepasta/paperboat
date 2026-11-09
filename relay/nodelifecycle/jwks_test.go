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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// trustRelay is a relay client observing a control plane whose trust document the test sets.
type trustRelay struct {
	t      *testing.T
	ctx    context.Context
	client *Client
	server *derpquic.Server
	mu     sync.Mutex
	doc    string
	pubs   map[string]string
}

func newTrustRelay(t *testing.T) *trustRelay {
	r := &trustRelay{t: t, pubs: map[string]string{}}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			NodeID     string `json:"node_id"`
			Expected   uint64 `json:"expected_generation"`
			Generation uint64 `json:"node_generation"`
			Epoch      string `json:"process_epoch"`
		}
		if json.NewDecoder(req.Body).Decode(&in) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		gen := in.Generation
		if req.URL.Path == "/v1/relay/nodes/start" {
			gen = in.Expected + 1
		}
		r.mu.Lock()
		out := Lease{NodeID: in.NodeID, Generation: gen, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 256, Revocations: []Revocation{}}
		if r.doc != "" {
			out.JWKS = json.RawMessage(r.doc)
		}
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(api.Close)
	c, err := New(Config{URL: api.URL, HTTP: api.Client(), Credential: strings.Repeat("test", 16), NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	lease, err := c.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	server, err := derpquic.NewServer(derpquic.Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: lease.Generation, ProcessEpoch: lease.ProcessEpoch, Keys: map[string]ed25519.PublicKey{"old": pub}})
	if err != nil {
		t.Fatal(err)
	}
	r.client, r.server, r.ctx = c, server, ctx
	return r
}

// key is the JWK of kid; the same kid always carries the same public key.
func (r *trustRelay) key(kid string) string {
	if r.pubs[kid] == "" {
		p, _, _ := ed25519.GenerateKey(rand.Reader)
		r.pubs[kid] = base64.RawURLEncoding.EncodeToString(p)
	}
	return `{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"` + kid + `","x":"` + r.pubs[kid] + `"}`
}

// document lists kids as keys; withdrawn is the raw withdrawn_key_ids JSON array or "".
func (r *trustRelay) document(withdrawn string, kids ...string) string {
	keys := make([]string, len(kids))
	for i, kid := range kids {
		keys[i] = r.key(kid)
	}
	if withdrawn != "" {
		withdrawn = `,"withdrawn_key_ids":` + withdrawn
	}
	return `{"keys":[` + strings.Join(keys, ",") + `]` + withdrawn + `}`
}

func (r *trustRelay) serve(doc string) {
	r.mu.Lock()
	r.doc = doc
	r.mu.Unlock()
}
func (r *trustRelay) observe() error { return r.client.Observe(r.ctx, r.server, false) }

// A signing-key rotation reaches a running relay with its next observation, no restart or re-enrollment:
// the new key is trusted next to the old one, a withdrawal drops the old one, a malformed document changes nothing.
func TestObservationAppliesIssuerTrustDocument(t *testing.T) {
	r := newTrustRelay(t)
	if err := r.observe(); err != nil || !slices.Equal(r.server.KeyIDs(), []string{"old"}) {
		t.Fatalf("an observation without a trust document changed the keys: %v %v", r.server.KeyIDs(), err)
	}
	r.serve(r.document("", "old", "new"))
	if err := r.observe(); err != nil || !slices.Equal(r.server.KeyIDs(), []string{"new", "old"}) {
		t.Fatalf("rotation not applied after one observation: %v %v", r.server.KeyIDs(), err)
	}
	r.serve(r.document(`["old"]`, "old", "new"))
	if err := r.observe(); err != nil || !slices.Equal(r.server.KeyIDs(), []string{"new"}) {
		t.Fatalf("withdrawal not applied: %v %v", r.server.KeyIDs(), err)
	}
	r.serve(`{"keys":[]}`)
	if err := r.observe(); !errors.Is(err, ErrControl) || !slices.Equal(r.server.KeyIDs(), []string{"new"}) {
		t.Fatalf("a malformed document replaced the keys: %v %v", r.server.KeyIDs(), err)
	}
}

// A malformed trust document fails closed: the observation errors and the last good key set stays.
func TestObservationRefusesMalformedIssuerTrustDocument(t *testing.T) {
	r := newTrustRelay(t)
	r.serve(r.document("", "good"))
	if err := r.observe(); err != nil || !slices.Equal(r.server.KeyIDs(), []string{"good"}) {
		t.Fatalf("setup: %v %v", r.server.KeyIDs(), err)
	}
	x := r.pubs["good"]
	raw, _ := base64.RawURLEncoding.DecodeString(x)
	// The last of 43 base64 chars carries 2 spare bits that must be zero; a lax decoder accepts them set.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	nonCanonical := x[:len(x)-1] + string(alphabet[strings.IndexByte(alphabet, x[len(x)-1])|1])
	jwk := func(old, new string) string { return strings.Replace(r.key("good"), old, new, 1) }
	manyIDs := make([]string, 129)
	for i := range manyIDs {
		manyIDs[i] = strconv.Quote("w" + strconv.Itoa(i))
	}
	cases := map[string]string{
		"duplicate kid":          `{"keys":[` + r.key("good") + `,` + r.key("good") + `]}`,
		"withdrawn over the cap": `{"keys":[` + r.key("good") + `],"withdrawn_key_ids":[` + strings.Join(manyIDs, ",") + `]}`,
		"empty withdrawn id":     r.document(`[""]`, "good"),
		"overlong withdrawn id":  r.document(`["`+strings.Repeat("a", 129)+`"]`, "good"),
		"bad base64":             `{"keys":[` + jwk(x, "!!!!") + `]}`,
		"short key":              `{"keys":[` + jwk(x, base64.RawURLEncoding.EncodeToString(raw[:31])) + `]}`,
		"long key":               `{"keys":[` + jwk(x, base64.RawURLEncoding.EncodeToString(append(raw, 0))) + `]}`,
		"padded base64":          `{"keys":[` + jwk(x, x+"=") + `]}`,
		"wrong kty":              `{"keys":[` + jwk("OKP", "RSA") + `]}`,
		"wrong crv":              `{"keys":[` + jwk("Ed25519", "P-256") + `]}`,
		"wrong use":              `{"keys":[` + jwk(`"sig"`, `"enc"`) + `]}`,
		"wrong alg":              `{"keys":[` + jwk("EdDSA", "RS256") + `]}`,
		"missing use":            `{"keys":[` + jwk(`"use":"sig",`, "") + `]}`,
		"overlong kid":           `{"keys":[` + jwk(`"good"`, `"`+strings.Repeat("k", 129)+`"`) + `]}`,
		"empty kid":              `{"keys":[` + jwk(`"good"`, `""`) + `]}`,
		"withdraws every key":    r.document(`["good"]`, "good"),
		"unknown field":          `{"keys":[` + r.key("good") + `],"extra":1}`,
		"nested beyond depth":    `{"keys":[` + r.key("good") + `],"withdrawn_key_ids":` + strings.Repeat("[", 70) + strings.Repeat("]", 70) + `}`,
		"non-canonical x":        `{"keys":[` + jwk(x, nonCanonical) + `]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			r.serve(doc)
			if err := r.observe(); !errors.Is(err, ErrControl) {
				t.Fatalf("observe = %v, want ErrControl", err)
			}
			if !slices.Equal(r.server.KeyIDs(), []string{"good"}) {
				t.Fatalf("keys changed to %v", r.server.KeyIDs())
			}
		})
	}
}
