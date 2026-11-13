package tlscert

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var persistNow = time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)

func persistentRegistry(t *testing.T, store Persistence, now time.Time, onError func(string, error)) *Registry {
	t.Helper()
	registry, err := NewRegistry(Config{Now: func() time.Time { return now }, Persistence: store, OnPersistError: onError})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func activateFor(t *testing.T, registry *Registry, binding Binding, bundle Bundle) {
	t.Helper()
	ctx := context.Background()
	for _, step := range []func() error{
		func() error { return registry.Stage(ctx, binding, bundle) },
		func() error { return registry.MarkReady(ctx, binding) },
		func() error { return registry.Activate(ctx, binding) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
}

func restoreFrom(t *testing.T, registry *Registry, store *DiskStore, nodeID string) ImportReport {
	t.Helper()
	records, unsafe, err := store.Load()
	if err != nil || unsafe != 0 {
		t.Fatalf("load: unsafe=%d err=%v", unsafe, err)
	}
	return registry.RestoreEncoded(context.Background(), nodeID, records)
}

func TestDiskStoreKeepsPrivateFilesAndSkipsUnsafeOnes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certificates")
	store, err := NewDiskStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, %v", info, err)
	}
	if err := store.Save("w:*.example.test", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("w:*.example.test", []byte("two")); err != nil { // replaces
		t.Fatal(err)
	}
	if info, err := os.Stat(store.path("w:*.example.test")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leftover.cert"), []byte("shared"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, persistedTemporary+"crashed"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, unsafe, err := store.Load()
	if err != nil || unsafe != 1 || len(records) != 1 || string(records[0]) != "two" {
		t.Fatalf("load = %q unsafe=%d err=%v", records, unsafe, err)
	}
	if _, err := os.Stat(filepath.Join(dir, persistedTemporary+"crashed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted save was not cleaned up: %v", err)
	}
	if err := store.Delete("w:*.example.test"); err != nil || store.Delete("w:*.example.test") != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDiskStore(dir); !errors.Is(err, ErrUnsafeStore) {
		t.Fatalf("open a world-readable directory: %v", err)
	}
}

// The deploy outage: a new edge process serves the previous process's certificate at once, and the server's delivery for
// the new epoch then replaces it through the usual fences, with no conflict and no leftover entry.
func TestRegistryRestoresPersistedCertificatesBeforeTheServerRedelivers(t *testing.T) {
	store, err := NewDiskStore(filepath.Join(t.TempDir(), "certificates"))
	if err != nil {
		t.Fatal(err)
	}
	oldBinding := testBinding("*.restart.example.test", 1)
	oldBundle := testBundle(t, oldBinding.Hostname, persistNow, 1)
	before := persistentRegistry(t, store, persistNow, nil)
	activateFor(t, before, oldBinding, oldBundle)
	if err := before.Close(); err != nil { // the process stops: the files stay
		t.Fatal(err)
	}

	after := persistentRegistry(t, store, persistNow, nil)
	if _, err := after.GetCertificate(tlsClientHello("a.restart.example.test")); !errors.Is(err, ErrCertificateMissing) {
		t.Fatalf("a fresh registry served %v", err)
	}
	if report := restoreFrom(t, after, store, oldBinding.EdgeNodeID); report.Imported != 1 || len(report.Dropped) != 0 {
		t.Fatalf("restore = %+v", report)
	}
	served, err := after.GetCertificate(tlsClientHello("a.restart.example.test"))
	if err != nil {
		t.Fatalf("restored certificate not served: %v", err)
	}
	if leaf, err := x509.ParseCertificate(served.Certificate[0]); err != nil || leaf.SerialNumber.Int64() != 1 {
		t.Fatalf("served the wrong certificate: %v", err)
	}

	// The server delivers the same certificate for the new process epoch (assignment generation moved on).
	newBinding := oldBinding
	newBinding.EdgeProcessEpoch = "epoch_0002"
	newBinding.AssignmentGeneration = 2
	activateFor(t, after, newBinding, oldBundle)
	if err := after.Retire(context.Background(), oldBinding); err != nil {
		t.Fatalf("a retire for the previous epoch must be a no-op: %v", err)
	}
	views := after.Snapshot()
	if len(views) != 1 || views[0].Binding.EdgeProcessEpoch != "epoch_0002" || views[0].State != StateActive {
		t.Fatalf("snapshot after redelivery = %+v", views)
	}
	records, _, err := store.Load()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %d, %v", len(records), err)
	}
	third := persistentRegistry(t, store, persistNow, nil)
	restoreFrom(t, third, store, oldBinding.EdgeNodeID)
	if views := third.Snapshot(); len(views) != 1 || views[0].Binding.EdgeProcessEpoch != "epoch_0002" {
		t.Fatalf("the replacement was not what got persisted: %+v", views)
	}
}

func TestRestoreDropsAndDeletesExpiredAndForeignCertificates(t *testing.T) {
	store, err := NewDiskStore(filepath.Join(t.TempDir(), "certificates"))
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding("old.example.test", 1)
	activateFor(t, persistentRegistry(t, store, persistNow, nil), binding, testBundle(t, binding.Hostname, persistNow, 1))

	later := persistentRegistry(t, store, persistNow.Add(48*time.Hour), nil) // past notAfter (24 h)
	if report := restoreFrom(t, later, store, binding.EdgeNodeID); report.Imported != 0 || report.Dropped["expired"] != 1 {
		t.Fatalf("expired restore = %+v", report)
	}
	if records, _, _ := store.Load(); len(records) != 0 {
		t.Fatalf("the expired record stayed on disk")
	}

	activateFor(t, persistentRegistry(t, store, persistNow, nil), binding, testBundle(t, binding.Hostname, persistNow, 2))
	other := persistentRegistry(t, store, persistNow, nil)
	if report := restoreFrom(t, other, store, "edge_other"); report.Imported != 0 || report.Dropped["other_node"] != 1 {
		t.Fatalf("foreign restore = %+v", report)
	}
	if _, err := other.GetCertificate(tlsClientHello(binding.Hostname)); !errors.Is(err, ErrCertificateMissing) {
		t.Fatalf("another node's certificate was served: %v", err)
	}
	if records, _, _ := store.Load(); len(records) != 0 {
		t.Fatalf("the foreign record stayed on disk")
	}
}

func TestRevokeDeletesThePersistedCertificate(t *testing.T) {
	store, err := NewDiskStore(filepath.Join(t.TempDir(), "certificates"))
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding("revoked.example.test", 1)
	registry := persistentRegistry(t, store, persistNow, nil)
	activateFor(t, registry, binding, testBundle(t, binding.Hostname, persistNow, 1))
	if records, _, _ := store.Load(); len(records) != 1 {
		t.Fatalf("an active certificate was not persisted")
	}
	if err := registry.Revoke(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if records, _, _ := store.Load(); len(records) != 0 {
		t.Fatalf("a revoked certificate stayed on disk")
	}
}

type failingPersistence struct{}

func (failingPersistence) Save(string, []byte) error { return errors.New("disk full") }
func (failingPersistence) Delete(string) error       { return errors.New("read-only") }

func TestPersistenceFailureNeverFailsTheActivation(t *testing.T) {
	var operations []string
	registry := persistentRegistry(t, failingPersistence{}, persistNow, func(operation string, _ error) { operations = append(operations, operation) })
	binding := testBinding("failing.example.test", 1)
	activateFor(t, registry, binding, testBundle(t, binding.Hostname, persistNow, 1))
	if _, err := registry.GetCertificate(tlsClientHello(binding.Hostname)); err != nil {
		t.Fatalf("the certificate stopped serving: %v", err)
	}
	if err := registry.Revoke(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 || operations[0] != "save" || operations[1] != "delete" {
		t.Fatalf("reported operations = %v", operations)
	}
}

// Jobs of one host key can finish out of order: a stale save, or a delete after a newer activation, is skipped.
func TestStalePersistJobsNeverOverwriteANewerCertificate(t *testing.T) {
	store, err := NewDiskStore(filepath.Join(t.TempDir(), "certificates"))
	if err != nil {
		t.Fatal(err)
	}
	registry := persistentRegistry(t, store, persistNow, nil)
	first := testBinding("race.example.test", 1)
	activateFor(t, registry, first, testBundle(t, first.Hostname, persistNow, 1))
	registry.mu.RLock()
	stale := registry.persistJobLocked(hostKey(first.Hostname, false), registry.entries[registry.active[hostKey(first.Hostname, false)]])
	registry.mu.RUnlock()
	second := first
	second.CertificateGeneration = 2
	activateFor(t, registry, second, testBundle(t, second.Hostname, persistNow, 2))

	registry.finishPersist(stale) // generation 1's save finishing after generation 2's
	registry.finishPersist(&persistJob{key: hostKey(first.Hostname, false), remove: true})
	records, _, err := store.Load()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %d, %v", len(records), err)
	}
	restored := persistentRegistry(t, store, persistNow, nil)
	restored.RestoreEncoded(context.Background(), first.EdgeNodeID, records)
	if views := restored.Snapshot(); len(views) != 1 || views[0].Binding.CertificateGeneration != 2 {
		t.Fatalf("the disk holds %+v, want generation 2", views)
	}
}
