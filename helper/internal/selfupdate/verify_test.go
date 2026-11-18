package selfupdate

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// signedRepository serves TUF metadata (consistent snapshot, one key per role) naming files as targets.
func signedRepository(t *testing.T, files map[string][]byte) (root []byte, server *httptest.Server) {
	t.Helper()
	keys := map[string]ed25519.PrivateKey{}
	rootMeta := metadata.Root(time.Now().UTC().Add(24 * time.Hour))
	rootMeta.Signed.ConsistentSnapshot = true
	for _, role := range []string{"root", "targets", "snapshot", "timestamp"} {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys[role] = private
		key, err := metadata.KeyFromPublicKey(private.Public())
		if err != nil {
			t.Fatal(err)
		}
		if err := rootMeta.Signed.AddKey(key, role); err != nil {
			t.Fatal(err)
		}
	}
	targets := metadata.Targets(time.Now().UTC().Add(24 * time.Hour))
	for name, body := range files {
		info, err := metadata.TargetFile().FromBytes(name, body, "sha256")
		if err != nil {
			t.Fatal(err)
		}
		custom := json.RawMessage(`{"repository":"example/paperboat","url":"https://releases.example.test/download/` + filepath.Base(name) + `"}`)
		info.Custom = &custom
		targets.Signed.Targets[name] = info
	}
	snapshot := metadata.Snapshot(time.Now().UTC().Add(24 * time.Hour))
	snapshot.Signed.Meta["targets.json"] = metadata.MetaFile(1)
	timestamp := metadata.Timestamp(time.Now().UTC().Add(time.Hour))
	timestamp.Signed.Meta["snapshot.json"] = metadata.MetaFile(1)
	for role, value := range map[string]interface {
		Sign(signature.Signer) (*metadata.Signature, error)
	}{"root": rootMeta, "targets": targets, "snapshot": snapshot, "timestamp": timestamp} {
		signer, err := signature.LoadSigner(keys[role], crypto.Hash(0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := value.Sign(signer); err != nil {
			t.Fatal(err)
		}
	}
	served := map[string][]byte{}
	for name, value := range map[string]interface{ ToBytes(bool) ([]byte, error) }{"/metadata/timestamp.json": timestamp, "/metadata/1.snapshot.json": snapshot, "/metadata/1.targets.json": targets} {
		body, err := value.ToBytes(false)
		if err != nil {
			t.Fatal(err)
		}
		served[name] = body
	}
	root, err := rootMeta.ToBytes(false)
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := served[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return root, server
}

// A bundle verifies only when the signed metadata names it with its exact length and sha256; a tampered bundle,
// a version that is not a signed target, and metadata signed by another root are refused before anything changes.
func TestVerifyAcceptsOnlyTheSignedBundle(t *testing.T) {
	bundle := []byte("self-host bundle 2026.11.19.1")
	root, server := signedRepository(t, map[string][]byte{TargetName("2026.11.19.1", "amd64"): bundle})
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	verify := func(root []byte, version, file string) error {
		return Verify(context.Background(), server.URL, root, t.TempDir(), version, "amd64", file, server.Client())
	}
	if err := verify(root, "2026.11.19.1", path); err != nil {
		t.Fatalf("signed bundle refused: %v", err)
	}
	if _, url, err := Target(context.Background(), server.URL, root, t.TempDir(), "2026.11.19.1", "amd64", server.Client()); err != nil || url != "https://releases.example.test/download/paperboat-selfhost-linux-amd64.tar.gz" {
		t.Fatalf("signed download URL = %q, %v", url, err)
	}
	tampered := filepath.Join(dir, "tampered.tar.gz")
	if err := os.WriteFile(tampered, append([]byte("x"), bundle[1:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(root, "2026.11.19.1", tampered); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered bundle: %v", err)
	}
	if err := verify(root, "2026.11.19.2", path); !errors.Is(err, ErrSignature) {
		t.Fatalf("unsigned version: %v", err)
	}
	otherRoot, _ := signedRepository(t, nil)
	if err := verify(otherRoot, "2026.11.19.1", path); !errors.Is(err, ErrSignature) {
		t.Fatalf("metadata under another root: %v", err)
	}
}
