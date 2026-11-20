package httpapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReleaseMetadataHealthReportsExpiringAndExpiredRoles(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	write := func(role string, expires time.Time) {
		path := filepath.Join(dir, "tuf", "metadata", role+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"signed":{"expires":"`+expires.Format(time.RFC3339)+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := releaseMetadataHealth(dir, now)["status"]; got != "failed" {
		t.Fatalf("missing metadata status = %v, want failed", got)
	}
	for role, in := range map[string]time.Duration{"timestamp": 24 * time.Hour, "snapshot": 7 * 24 * time.Hour, "targets": 90 * 24 * time.Hour, "root": 700 * 24 * time.Hour} {
		write(role, now.Add(in))
	}
	if got := releaseMetadataHealth(dir, now)["status"]; got != "healthy" {
		t.Fatalf("fresh metadata status = %v, want healthy", got)
	}
	write("timestamp", now.Add(11*time.Hour)) // the 6 h refresh has stopped for 13 h
	if got := releaseMetadataHealth(dir, now); got["status"] != "degraded" || got["code"] != "release_metadata_expiring_timestamp" {
		t.Fatalf("stale timestamp health = %v", got)
	}
	write("targets", now.Add(-time.Hour))
	if got := releaseMetadataHealth(dir, now)["status"]; got != "failed" {
		t.Fatalf("expired targets status = %v, want failed", got)
	}
}
