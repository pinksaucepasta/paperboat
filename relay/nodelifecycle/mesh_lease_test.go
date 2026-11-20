package nodelifecycle

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// meshAPI is a control plane for one relay: start and observe answer a lease whose mesh_peers is the raw JSON set
// with setPeers (nil omits the field), and the body of the last request is kept for the test to read.
type meshAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	peers json.RawMessage
	last  []byte
}

func newMeshAPI(t *testing.T) *meshAPI {
	t.Helper()
	a := &meshAPI{}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
		var in struct {
			NodeID     string `json:"node_id"`
			Expected   uint64 `json:"expected_generation"`
			Generation uint64 `json:"node_generation"`
			Epoch      string `json:"process_epoch"`
		}
		if err != nil || json.Unmarshal(body, &in) != nil {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		gen := in.Generation
		if r.URL.Path == "/v1/relay/nodes/start" {
			gen = in.Expected + 1
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.last = body
		json.NewEncoder(w).Encode(Lease{NodeID: in.NodeID, Generation: gen, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(),
			Revocations: []Revocation{}, MeshPeers: a.peers})
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *meshAPI) setPeers(raw json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.peers = raw
}

// bodyFields returns the top-level fields of the last request.
func (a *meshAPI) bodyFields(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(a.last, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// newMeshClient starts a relay against api and returns its client and first lease.
func newMeshClient(t *testing.T, api *meshAPI) (*Client, Lease) {
	t.Helper()
	c, err := New(Config{URL: api.srv.URL, HTTP: api.srv.Client(), Credential: strings.Repeat("test", 16), NodeID: "relay", ExpectedGeneration: 1,
		StatePath: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	lease, err := c.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return c, lease
}

// newMeshServer is the relay's serving side for lease; withMesh attaches a mesh, which is not started so it dials nothing.
func newMeshServer(t *testing.T, api *meshAPI, lease Lease, withMesh bool) *derpquic.Server {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	server, err := derpquic.NewServer(derpquic.Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: lease.Generation, ProcessEpoch: lease.ProcessEpoch,
		Keys: map[string]ed25519.PublicKey{"test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	if withMesh {
		mesh, err := derpquic.NewMesh(derpquic.MeshConfig{Cert: api.srv.TLS.Certificates[0]})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.SetMesh(mesh); err != nil {
			t.Fatal(err)
		}
	}
	return server
}

// meshPeer is a peer relay of the test mesh; its pin is a digest of its node id, a valid pin without a real certificate.
func meshPeer(id string) nodemesh.Peer {
	sum := sha256.Sum256([]byte(id))
	return nodemesh.Peer{NodeID: id, Region: "test", Host: "192.0.2.10", Port: 443, SPKISHA256: "sha256:" + hex.EncodeToString(sum[:])}
}

func peersJSON(t *testing.T, peers ...nodemesh.Peer) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(peers)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// syncBuffer collects the relay's log lines; the observation logs from whichever goroutine runs it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// A lease's mesh_peers replaces the relay's directory: the peers its links dial and accept from.
func TestLeaseCarriesMeshPeers(t *testing.T) {
	api := newMeshAPI(t)
	peers := []nodemesh.Peer{meshPeer("b"), meshPeer("c")}
	api.setPeers(peersJSON(t, peers...))
	c, lease := newMeshClient(t, api)
	server := newMeshServer(t, api, lease, true)
	if err := c.Observe(t.Context(), server, false); err != nil {
		t.Fatal(err)
	}
	if got := server.MeshPeers(); !slices.Equal(got, peers) {
		t.Fatalf("directory %v, want %v", got, peers)
	}
}

// An absent mesh_peers means no mesh: the directory is cleared, and observations keep applying, with or without a mesh.
func TestLeaseWithoutMeshPeersClearsNothingBreaking(t *testing.T) {
	api := newMeshAPI(t)
	api.setPeers(peersJSON(t, meshPeer("b")))
	c, lease := newMeshClient(t, api)
	server := newMeshServer(t, api, lease, true)
	if err := c.Observe(t.Context(), server, false); err != nil {
		t.Fatal(err)
	}
	if len(server.MeshPeers()) != 1 {
		t.Fatalf("directory not applied: %v", server.MeshPeers())
	}
	api.setPeers(nil)
	if err := c.Observe(t.Context(), server, false); err != nil {
		t.Fatalf("observation without mesh_peers: %v", err)
	}
	if got := server.MeshPeers(); len(got) != 0 {
		t.Fatalf("absent mesh_peers left %v", got)
	}
	bare := newMeshServer(t, api, lease, false)
	if err := c.Observe(t.Context(), bare, false); err != nil {
		t.Fatalf("observation of a relay without a mesh: %v", err)
	}
}

// A malformed mesh_peers (wrong type, an invalid peer, a duplicate node, a duplicate pin) keeps the last good directory,
// is reported, and does not fail the observation; a later good list applies again.
func TestMalformedMeshPeersKeepLastGoodSet(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	api := newMeshAPI(t)
	good := []nodemesh.Peer{meshPeer("b")}
	api.setPeers(peersJSON(t, good...))
	c, lease := newMeshClient(t, api)
	server := newMeshServer(t, api, lease, true)
	if err := c.Observe(t.Context(), server, false); err != nil {
		t.Fatal(err)
	}
	samePin := meshPeer("c")
	samePin.SPKISHA256 = good[0].SPKISHA256
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"wrong type", json.RawMessage(`"b"`)},
		{"invalid peer", peersJSON(t, nodemesh.Peer{NodeID: "c"})},
		{"duplicate node", peersJSON(t, meshPeer("c"), meshPeer("c"))},
		{"duplicate pin", peersJSON(t, good[0], samePin)},
	} {
		logs.reset()
		api.setPeers(tc.raw)
		if err := c.Observe(t.Context(), server, false); err != nil {
			t.Fatalf("%s: observation failed: %v", tc.name, err)
		}
		if got := server.MeshPeers(); !slices.Equal(got, good) {
			t.Fatalf("%s: directory %v, want the last good %v", tc.name, got, good)
		}
		if !strings.Contains(logs.String(), "keeping the last one") {
			t.Fatalf("%s: not reported: %q", tc.name, logs.String())
		}
	}
	next := []nodemesh.Peer{meshPeer("d")}
	api.setPeers(peersJSON(t, next...))
	if err := c.Observe(t.Context(), server, false); err != nil {
		t.Fatal(err)
	}
	if got := server.MeshPeers(); !slices.Equal(got, next) {
		t.Fatalf("a later good list did not apply: %v", got)
	}
}

// An observation carries the mesh counters as mesh when the relay has a mesh, and omits the field when it has none.
func TestObserveBodyCarriesMeshStats(t *testing.T) {
	api := newMeshAPI(t)
	c, lease := newMeshClient(t, api)
	withMesh := newMeshServer(t, api, lease, true)
	if err := c.Observe(t.Context(), withMesh, false); err != nil {
		t.Fatal(err)
	}
	raw, ok := api.bodyFields(t)["mesh"]
	if !ok {
		t.Fatal("observation of a relay with a mesh has no mesh field")
	}
	var got nodemesh.Stats
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if want, _ := withMesh.MeshStats(); got != want {
		t.Fatalf("mesh stats %+v, want %+v", got, want)
	}
	bare := newMeshServer(t, api, lease, false)
	if err := c.Observe(t.Context(), bare, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.bodyFields(t)["mesh"]; ok {
		t.Fatal("observation of a relay without a mesh carries mesh stats")
	}
}

// Mesh trust ends with the control plane's word: after MeshPeersStaleAfter without a lease that carried a valid
// directory (control unreachable, or the list stays malformed) the directory is cleared, and a good lease restores it.
func TestMeshStaleLeaseExpiresPeers(t *testing.T) {
	api := newMeshAPI(t)
	good := []nodemesh.Peer{meshPeer("b")}
	api.setPeers(peersJSON(t, good...))
	c, lease := newMeshClient(t, api)
	c.cfg.MeshPeersStaleAfter = 300 * time.Millisecond
	server := newMeshServer(t, api, lease, true)
	if err := c.Observe(t.Context(), server, false); err != nil {
		t.Fatal(err)
	}
	api.srv.Close() // control unreachable: observations fail
	if err := c.Observe(t.Context(), server, false); err == nil {
		t.Fatal("observation against a closed control plane succeeded")
	}
	if got := server.MeshPeers(); !slices.Equal(got, good) {
		t.Fatalf("directory %v dropped before it was stale, want %v", got, good)
	}
	time.Sleep(350 * time.Millisecond)
	if err := c.Observe(t.Context(), server, false); err == nil {
		t.Fatal("observation against a closed control plane succeeded")
	}
	if got := server.MeshPeers(); len(got) != 0 {
		t.Fatalf("stale directory %v still trusted", got)
	}
}

// A lease that keeps carrying an invalid directory does not renew the last good one forever either.
func TestMeshStaleMalformedLeaseExpiresPeers(t *testing.T) {
	api := newMeshAPI(t)
	api.setPeers(peersJSON(t, meshPeer("b")))
	c, lease := newMeshClient(t, api)
	c.cfg.MeshPeersStaleAfter = 300 * time.Millisecond
	server := newMeshServer(t, api, lease, true)
	if err := c.Observe(t.Context(), server, false); err != nil {
		t.Fatal(err)
	}
	api.setPeers(json.RawMessage(`"b"`))
	if err := c.Observe(t.Context(), server, false); err != nil || len(server.MeshPeers()) != 1 {
		t.Fatalf("a fresh directory is kept: err %v, peers %v", err, server.MeshPeers())
	}
	time.Sleep(350 * time.Millisecond)
	if err := c.Observe(t.Context(), server, false); err != nil {
		t.Fatal(err)
	}
	if got := server.MeshPeers(); len(got) != 0 {
		t.Fatalf("stale directory %v still trusted", got)
	}
}
