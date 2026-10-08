//go:build !windows

package diagnostics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskFailureIsReportedByFlushAndRetainedWithBoundedState(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "diagnostics")
	ring, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	defer ring.Close()
	if err := os.Rename(directory, directory+".offline"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for i := 0; i < 32; i++ {
		if err := ring.Record(testEvent(t, "failed")); err != nil {
			t.Fatal(err)
		}
		if err := ring.Flush(ctx); err == nil {
			t.Fatal("flush reported success after persistence failed")
		}
	}
	stats := ring.Stats()
	if stats.FailedRecords != 32 || stats.DroppedRecords != 32 || stats.PersistedBytes != 0 {
		t.Fatalf("missing persistence-loss evidence: %+v", stats)
	}
	ring.mu.Lock()
	last := ring.err
	ring.mu.Unlock()
	nodes := 0
	remaining := []error{last}
	for len(remaining) != 0 && nodes <= 8 {
		err := remaining[0]
		remaining = remaining[1:]
		if err == nil {
			continue
		}
		nodes++
		switch err := err.(type) {
		case interface{ Unwrap() []error }:
			remaining = append(remaining, err.Unwrap()...)
		case interface{ Unwrap() error }:
			remaining = append(remaining, err.Unwrap())
		}
	}
	if nodes > 8 {
		t.Fatal("disk outage retained an accumulating error tree")
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ring.Record(testEvent(t, "recovered")); err != nil {
		t.Fatal(err)
	}
	// Historical loss remains visible, while new events can persist normally.
	if err := ring.Flush(ctx); err == nil {
		t.Fatal("previous record loss disappeared from flush evidence")
	}
	data, err := ring.ReadAll(ctx, DefaultMaximumBytes)
	if err != nil || bytesLines(data) != 1 || ring.Stats().PersistedBytes == 0 {
		t.Fatalf("disk logging did not recover: lines=%d err=%v", bytesLines(data), err)
	}
}

func TestDiskRetentionExpiresRecordsWithoutRestart(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "diagnostics")
	now := time.Now().UTC()
	ring, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid(), Retention: time.Hour, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer ring.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := ring.Record(testEvent(t, "expired")); err != nil {
		t.Fatal(err)
	}
	if err := ring.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	entries, _, err := ring.segments()
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one segment: %v %v", entries, err)
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(entries[0].path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := ring.Record(testEvent(t, "current")); err != nil {
		t.Fatal(err)
	}
	if err := ring.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := ring.ReadAll(ctx, DefaultMaximumBytes)
	if err != nil || bytesLines(data) != 1 {
		t.Fatalf("expired record retained without restart: lines=%d err=%v", bytesLines(data), err)
	}
}
