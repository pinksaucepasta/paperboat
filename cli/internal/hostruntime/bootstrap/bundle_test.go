package bootstrap

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These checks own the offline transport contract: proof authenticity, receiver
// rollback floors and cleanup. Existing artifact tests own ordinary HTTP faults.
func TestUpdateBundleOfflineAuthentication(t *testing.T) {
	now := time.Now().UTC()
	body := []byte("unchanged official bytes")
	repository := newTestTUFRepository(t, body, "2026.10.11.1", now.Add(time.Hour))
	server := repository.server(t)
	path := filepath.Join(t.TempDir(), "release.pb-update")
	if _, err := exportUpdateBundle(context.Background(), server.URL, filepath.Join(t.TempDir(), "state"), path, server.Client(), now, repository.root, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	origin := server.URL
	server.Close()
	// Any accidental use of the process-wide network fallback fails this check.
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("offline import attempted HTTP")
		return nil, errors.New("network forbidden")
	})
	defer func() { http.DefaultTransport = old }()
	receiver := filepath.Join(t.TempDir(), "receiver")
	result, err := importUpdateBundle(context.Background(), origin, receiver, path, filepath.Join(t.TempDir(), "staged"), now, repository.root, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(result.ArtifactPath)
	if err != nil || !bytes.Equal(got, body) || result.Index.Version != "2026.10.11.1" {
		t.Fatalf("artifact=%q result=%+v err=%v", got, result, err)
	}
	if err = secureArtifactFile(result.ArtifactPath); err != nil {
		t.Fatal(err)
	}
	os.Remove(result.ArtifactPath)
	cached, err := fetchCachedVerifiedReleaseIndex(context.Background(), origin, receiver, now, repository.root, runtime.GOOS, runtime.GOARCH)
	if err != nil || cached.Version != result.Index.Version {
		t.Fatalf("offline receiver recovery policy: %s, %v", cached.Version, err)
	}
	for _, kind := range []string{"artifact", "proof", "missing", "duplicate", "traversal", "link", "oversized", "wrong-platform", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			candidate := path
			if kind != "wrong-platform" && kind != "cancelled" {
				candidate = filepath.Join(t.TempDir(), "modified.pb-update")
				rewriteBundle(t, path, candidate, kind)
			}
			ctx := context.Background()
			if kind == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			platform, architecture := runtime.GOOS, runtime.GOARCH
			if kind == "wrong-platform" {
				platform, architecture = "windows", "arm64"
				if runtime.GOOS == platform && runtime.GOARCH == architecture {
					platform, architecture = "linux", "amd64"
				}
			}
			destination := filepath.Join(t.TempDir(), "staged")
			if _, err := importUpdateBundle(ctx, origin, filepath.Join(t.TempDir(), "receiver"), candidate, destination, now, repository.root, platform, architecture); err == nil {
				t.Fatal("invalid bundle accepted")
			}
			entries, err := os.ReadDir(destination)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("partial artifacts remain: %v", entries)
			}
		})
	}
}
func rewriteBundle(t *testing.T, source, destination, kind string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(data))
	f, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "missing-root" && strings.Contains(h.Name, ".root.json") {
			continue
		}
		if kind == "missing" && strings.Contains(h.Name, "targets.json") {
			continue
		}
		if kind == "artifact" && h.Name == bundleArtifact {
			body[0] ^= 1
		}
		if kind == "root-proof" && strings.HasSuffix(h.Name, ".root.json") {
			body[0] ^= 1
		}
		if kind == "proof" && h.Name == "metadata/timestamp.json" {
			body[0] ^= 1
		}
		if kind == "link" && h.Name == bundleArtifact {
			h.Typeflag = tar.TypeSymlink
			h.Linkname = "outside"
			h.Size = 0
			body = nil
		}
		if kind == "oversized" && h.Name == bundleArtifact {
			h.Size = (512 << 20) + 1
			if err = tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			break
		}
		if kind == "traversal" && h.Name == bundleArtifact {
			h.Name = "../artifact"
		}
		if err = tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(body); err != nil {
			t.Fatal(err)
		}
		if kind == "duplicate" && h.Name == bundleArtifact {
			if err = tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if _, err = tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tw.Close(); err != nil && kind != "oversized" {
		t.Fatal(err)
	}
}
func TestUpdateBundleRootContinuityAndReceiverFloors(t *testing.T) {
	now := time.Now().UTC()
	keys := map[string]ed25519.PrivateKey{}
	old := newTestTUFRepositoryForMetadataVersion(t, []byte("old"), "2026.10.11.1", now.Add(time.Hour), runtime.GOOS, runtime.GOARCH, keys, 1)
	rotatedKeys := map[string]ed25519.PrivateKey{}
	for role, key := range keys {
		rotatedKeys[role] = key
	}
	_, timestampKey, keyErr := ed25519.GenerateKey(rand.Reader)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	rotatedKeys["timestamp"] = timestampKey
	newer := newTestTUFRepositoryForMetadataVersion(t, []byte("new"), "2026.10.11.2", now.Add(time.Hour), runtime.GOOS, runtime.GOARCH, rotatedKeys, 2)
	newer.files["/metadata/2.root.json"] = newer.root
	receiver := filepath.Join(t.TempDir(), "receiver")
	export := func(repo testTUFRepository) (string, string) {
		t.Helper()
		server := repo.server(t)
		defer server.Close()
		path := filepath.Join(t.TempDir(), "release.pb-update")
		if _, err := exportUpdateBundle(context.Background(), server.URL, filepath.Join(t.TempDir(), "sender"), path, server.Client(), now, old.root, runtime.GOOS, runtime.GOARCH); err != nil {
			t.Fatal(err)
		}
		return path, server.URL
	}
	oldPath, _ := export(old)
	newPath, origin := export(newer)
	missingRoot := filepath.Join(t.TempDir(), "missing-root.pb-update")
	rewriteBundle(t, newPath, missingRoot, "missing-root")
	if _, err := importUpdateBundle(context.Background(), origin, filepath.Join(t.TempDir(), "missing-root-receiver"), missingRoot, filepath.Join(t.TempDir(), "missing-root-staged"), now, old.root, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("rotation accepted without continuity proof")
	}
	result, err := importUpdateBundle(context.Background(), origin, receiver, newPath, filepath.Join(t.TempDir(), "staged"), now, old.root, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(result.ArtifactPath)
	// Released receivers already persisted accepted root v2 but did not retain
	// rotation history. Import must repair continuity using verified bundle
	// roots while preserving the receiver's existing metadata floors.
	history := filepath.Join(receiver, "roots", "2.root.json")
	if err := os.Remove(history); err != nil {
		t.Fatal(err)
	}
	floorPath := filepath.Join(receiver, "metadata", "timestamp.json")
	floorBefore, err := os.ReadFile(floorPath)
	if err != nil {
		t.Fatal(err)
	}
	tamperedRoot := filepath.Join(t.TempDir(), "tampered-root.pb-update")
	rewriteBundle(t, newPath, tamperedRoot, "root-proof")
	for _, bad := range []string{missingRoot, tamperedRoot, oldPath} {
		destination := filepath.Join(t.TempDir(), "rejected-rotation")
		if _, err := importUpdateBundle(context.Background(), origin, receiver, bad, destination, now, old.root, runtime.GOOS, runtime.GOARCH); err == nil {
			t.Fatal("missing or tampered continuity accepted on established receiver")
		}
		floorAfter, err := os.ReadFile(floorPath)
		if err != nil || !bytes.Equal(floorBefore, floorAfter) {
			t.Fatalf("failed continuity changed receiver floor: %v", err)
		}
		entries, _ := os.ReadDir(destination)
		if len(entries) != 0 {
			t.Fatal("failed continuity leaked artifact")
		}
	}
	cachedRootPath := filepath.Join(receiver, "metadata", "root.json")
	cachedRootBefore, err := os.ReadFile(cachedRootPath)
	if err != nil {
		t.Fatal(err)
	}
	alternateKeys := map[string]ed25519.PrivateKey{}
	for role, key := range rotatedKeys {
		alternateKeys[role] = key
	}
	_, alternateTimestamp, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	alternateKeys["timestamp"] = alternateTimestamp
	alternate := newTestTUFRepositoryForMetadataVersion(t, []byte("alternate"), "2026.10.11.2", now.Add(time.Hour), runtime.GOOS, runtime.GOARCH, alternateKeys, 2)
	if err := os.WriteFile(cachedRootPath, alternate.root, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := importUpdateBundle(context.Background(), origin, receiver, newPath, filepath.Join(t.TempDir(), "equivocation"), now, old.root, runtime.GOOS, runtime.GOARCH); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("same-version root equivocation accepted: %v", err)
	}
	if err := os.WriteFile(cachedRootPath, cachedRootBefore, 0600); err != nil {
		t.Fatal(err)
	}
	// Repeat import from disk must retain root v2 even though the bundle carries
	// the full chain from embedded v1. Then older metadata cannot reset the floor.
	result, err = importUpdateBundle(context.Background(), origin, receiver, newPath, filepath.Join(t.TempDir(), "repeat"), now, old.root, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(result.ArtifactPath)
	destination := filepath.Join(t.TempDir(), "rejected")
	if _, err = importUpdateBundle(context.Background(), origin, receiver, oldPath, destination, now, old.root, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("receiver accepted rollback")
	}
	entries, _ := os.ReadDir(destination)
	if len(entries) != 0 {
		t.Fatal("rollback left staged bytes")
	}
	// Exporting later from the same accepted state must still provide v1→v2
	// continuity for a fresh receiver, even with no rotation fetched this time.
	server := newer.server(t)
	defer server.Close()
	if err := os.Remove(history); err != nil {
		t.Fatal(err)
	}
	later := filepath.Join(t.TempDir(), "later.pb-update")
	if _, err = exportUpdateBundle(context.Background(), server.URL, receiver, later, server.Client(), now, old.root, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	result, err = importUpdateBundle(context.Background(), server.URL, filepath.Join(t.TempDir(), "fresh"), later, filepath.Join(t.TempDir(), "fresh-staged"), now, old.root, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(result.ArtifactPath)
}
func TestUpdateBundleExpiredProofRejected(t *testing.T) {
	now := time.Now().UTC()
	repo := newTestTUFRepository(t, []byte("old bytes"), "2026.10.11.1", now.Add(-time.Hour))
	path := filepath.Join(t.TempDir(), "expired.pb-update")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, data := range repo.files {
		if !strings.HasPrefix(name, "/metadata/") {
			continue
		}
		if err = tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(name, "/"), Mode: 0600, Typeflag: tar.TypeReg, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("old bytes")
	tw.WriteHeader(&tar.Header{Name: bundleArtifact, Mode: 0600, Typeflag: tar.TypeReg, Size: int64(len(body))})
	tw.Write(body)
	tw.Close()
	f.Close()
	dest := filepath.Join(t.TempDir(), "staged")
	if _, err = importUpdateBundle(context.Background(), "https://official.invalid", filepath.Join(t.TempDir(), "receiver"), path, dest, now, repo.root, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("expired proof accepted")
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		t.Fatal("expired proof left staged bytes")
	}
	cached := filepath.Join(t.TempDir(), "receiver")
	metadataDir := filepath.Join(cached, "metadata")
	if err := os.MkdirAll(metadataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "root.json"), repo.root, 0600); err != nil {
		t.Fatal(err)
	}
	for remote, data := range repo.files {
		name := strings.TrimPrefix(remote, "/metadata/")
		if name == remote {
			continue
		}
		if name == "1.snapshot.json" {
			name = "snapshot.json"
		}
		if name == "1.targets.json" {
			name = "targets.json"
		}
		if name != "timestamp.json" && name != "snapshot.json" && name != "targets.json" {
			continue
		}
		if err := os.WriteFile(filepath.Join(metadataDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fetchCachedVerifiedReleaseIndex(context.Background(), "https://official.invalid", cached, now, repo.root, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("expired retained proof accepted for offline recovery")
	}
}

type cancelBundleBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBundleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.cancel()
	return n, err
}
func TestUpdateBundleExportCancellationCleansStaging(t *testing.T) {
	now := time.Now().UTC()
	repo := newTestTUFRepository(t, []byte("official bytes"), "2026.10.11.1", now.Add(time.Hour))
	server := repo.server(t)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := *server.Client()
	base := client.Transport
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(request)
		if err == nil && request.URL.Hostname() == "github.com" {
			response.Body = &cancelBundleBody{ReadCloser: response.Body, cancel: cancel}
		}
		return response, err
	})
	state := filepath.Join(t.TempDir(), "state")
	destination := filepath.Join(t.TempDir(), "release.pb-update")
	if _, err := exportUpdateBundle(ctx, server.URL, state, destination, &client, now, repo.root, runtime.GOOS, runtime.GOARCH); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled export published output: %v", err)
	}
	for _, dir := range []string{state, filepath.Dir(destination)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".bundle-export-") || strings.HasPrefix(entry.Name(), ".pb-update-") {
				t.Fatalf("temporary staging remains: %s", entry.Name())
			}
		}
	}
}

func TestUpdateBundleStoreContentionCancellation(t *testing.T) {
	now := time.Now().UTC()
	repository := newTestTUFRepository(t, []byte("official bytes"), "2026.10.11.1", now.Add(time.Hour))
	server := repository.server(t)
	defer server.Close()
	state := filepath.Join(t.TempDir(), "state")
	unlock, err := lockArtifactState(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	output := filepath.Join(t.TempDir(), "release.pb-update")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := exportUpdateBundle(ctx, server.URL, state, output, server.Client(), now, repository.root, runtime.GOOS, runtime.GOARCH); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "metadata")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked caller mutated metadata: %v", err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled output: %v", err)
	}
	unlock()
	if _, err := exportUpdateBundle(context.Background(), server.URL, state, output, server.Client(), now, repository.root, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("store unusable after cancelled waiter: %v", err)
	}
	imported, err := importUpdateBundle(context.Background(), server.URL, state, output, filepath.Join(t.TempDir(), "staged"), now, repository.root, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(imported.ArtifactPath)
}

func TestUpdateBundleExportPreservesOutputCreatedDuringDownload(t *testing.T) {
	now := time.Now().UTC()
	repo := newTestTUFRepository(t, []byte("official bytes"), "2026.10.11.1", now.Add(time.Hour))
	server := repo.server(t)
	defer server.Close()
	directory := t.TempDir()
	destination := filepath.Join(directory, "release.pb-update")
	existing := []byte("another export owns this output")
	client := *server.Client()
	base := client.Transport
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(request)
		if err == nil && strings.Contains(request.URL.Path, "/releases/download/") {
			if writeErr := os.WriteFile(destination, existing, 0600); writeErr != nil {
				t.Error(writeErr)
			}
		}
		return response, err
	})
	_, err := exportUpdateBundle(context.Background(), server.URL, filepath.Join(t.TempDir(), "state"), destination, &client, now, repo.root, runtime.GOOS, runtime.GOARCH)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("occupied output error = %v, want file exists", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, existing) {
		t.Fatalf("existing output changed: %q, %v", got, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(destination) {
		t.Fatalf("export left temporary files: %v, %v", entries, err)
	}
}
