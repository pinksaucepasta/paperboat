package releases

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpiryWarningsFireInsideEachRolesWindow(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	fresh := map[string]time.Time{"timestamp": now.Add(24 * time.Hour), "snapshot": now.Add(7 * 24 * time.Hour), "targets": now.Add(90 * 24 * time.Hour), "root": now.Add(700 * 24 * time.Hour)}
	if w := ExpiryWarnings(fresh, now); len(w) != 0 {
		t.Fatalf("freshly signed metadata warned: %+v", w)
	}
	stale := map[string]time.Time{"timestamp": now.Add(11 * time.Hour), "snapshot": now.Add(7 * 24 * time.Hour), "targets": now.Add(13 * 24 * time.Hour), "root": now.Add(-time.Hour)}
	w := ExpiryWarnings(stale, now)
	if len(w) != 3 || w[0].Role != "root" || !w[0].Expired || w[1].Role != "timestamp" || w[2].Role != "targets" || w[2].Expired {
		t.Fatalf("warnings = %+v", w)
	}
}

func TestReadMetadataExpiryReadsEveryRole(t *testing.T) {
	dir := t.TempDir()
	metadata := filepath.Join(dir, "tuf", "metadata")
	if err := os.MkdirAll(metadata, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"root", "targets", "snapshot", "timestamp"} {
		if err := os.WriteFile(filepath.Join(metadata, role+".json"), []byte(`{"signed":{"expires":"2027-01-02T03:04:05Z"},"signatures":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadMetadataExpiry(dir)
	if err != nil || len(got) != 4 || !got["targets"].Equal(time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("expiries = %v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "timestamp.json"), []byte(`{"signed":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMetadataExpiry(dir); err == nil {
		t.Fatal("metadata without an expiry was accepted")
	}
}
