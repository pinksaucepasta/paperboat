package diagnostics

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func bundleEntry(t *testing.T, bundle Bundle, name string) []byte {
	t.Helper()
	archive, err := zip.OpenReader(bundle.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if entry.Name != name {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal(errors.Join(readErr, closeErr))
		}
		return data
	}
	t.Fatalf("missing bundle entry %s", name)
	return nil
}

func TestBundleExportsMemoryWhenPersistenceUnavailable(t *testing.T) {
	local := NewMemoryRecorder()
	if err := local.Record("process", "diagnostic_storage_unavailable", "warning", nil); err != nil {
		t.Fatal(err)
	}
	bundle, err := CreateBundle(context.Background(), BundleConfig{Directory: filepath.Join(t.TempDir(), "bundles"), OwnerUID: os.Geteuid(), Recorder: local, Status: json.RawMessage(`{"state":"degraded"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var manifest bundleManifest
	if err := json.Unmarshal(bundleEntry(t, bundle, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.PersistenceAvailable || manifest.PersistentFlushSucceeded || manifest.PersistentEventsAvailable || len(bundleEntry(t, bundle, "recent-events.ndjson")) == 0 || len(bundleEntry(t, bundle, "events.ndjson")) != 0 {
		t.Fatal("memory-only bundle implied persistent completeness or lost recent evidence")
	}
}

func TestBundleRemainsUsefulDuringStorageFailureAndAfterRecovery(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "events")
	local, err := NewRecorder(DiskConfig{Directory: path, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if err := local.Record("daemon", "ready", "info", nil); err != nil {
		t.Fatal(err)
	}
	if err := local.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".offline"); err != nil {
		t.Fatal(err)
	}
	if err := local.Record("daemon", "storage_failed", "warning", nil); err != nil {
		t.Fatal(err)
	}
	if err := local.Flush(context.Background()); err == nil {
		t.Fatal("storage loss was hidden")
	}
	if local.Stats().PersistenceAvailable {
		t.Fatal("failed persistence reported available")
	}
	config := BundleConfig{Directory: filepath.Join(root, "bundles"), OwnerUID: os.Geteuid(), Recorder: local, Status: json.RawMessage(`{"state":"degraded"}`)}
	bundle, err := CreateBundle(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	var manifest bundleManifest
	if err := json.Unmarshal(bundleEntry(t, bundle, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.PersistentFlushSucceeded || manifest.PersistentEventsAvailable || manifest.FailedRecords != 1 || manifest.PersistenceError != "not_found" {
		t.Fatalf("manifest=%#v", manifest)
	}
	if len(bundleEntry(t, bundle, "recent-events.ndjson")) == 0 {
		t.Fatal("outage bundle lost memory evidence")
	}
	if err := os.Rename(path+".offline", path); err != nil {
		t.Fatal(err)
	}
	if err := local.Record("daemon", "recovered", "info", nil); err != nil {
		t.Fatal(err)
	}
	if err := local.Flush(context.Background()); err == nil {
		t.Fatal("historical loss was hidden")
	}
	if !local.Stats().PersistenceAvailable {
		t.Fatal("persistence recovery was not visible")
	}
	bundle, err = CreateBundle(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bundleEntry(t, bundle, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.PersistenceAvailable || !manifest.PersistentEventsAvailable || manifest.PersistentFlushSucceeded || manifest.FailedRecords != 1 || len(bundleEntry(t, bundle, "events.ndjson")) == 0 {
		t.Fatalf("recovery manifest=%#v", manifest)
	}
}
