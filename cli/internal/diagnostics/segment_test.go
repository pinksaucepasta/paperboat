package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilesystemValidationRetainsMissingCauseWithoutDisclosingPath(t *testing.T) {
	owner, err := resolveDiagnosticOwner(DiskConfig{OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private-record-does-not-exist")
	_, err = verifiedDiagnosticFile(path, owner)
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), path) {
		t.Fatal("filesystem validation lost cause or disclosed path")
	}
	fault := errorreport.ProjectFault(context.Background(), "pb", "diagnostic", "command", "command_failed", err)
	if fault.Stage != "diagnostic_storage" || fault.Code != "diagnostic_storage_unavailable" || fault.Cause != "not_found" {
		t.Fatalf("fault=%#v", fault)
	}
}

func TestSegmentCreationNeverReplacesExistingEvidence(t *testing.T) {
	config := DiskConfig{Directory: filepath.Join(t.TempDir(), "events"), OwnerUID: os.Geteuid()}
	owner, err := resolveDiagnosticOwner(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureDiagnosticDirectory(config.Directory, owner); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.Directory, "events-00000000000000000001-00.ndjson")
	if err := createDiagnosticSegment(path, owner); err != nil {
		t.Fatal(err)
	}
	file, err := openDiagnosticAppend(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("existing evidence\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := createDiagnosticSegment(path, owner); !errors.Is(err, os.ErrExist) {
		t.Fatalf("segment collision did not refuse replacement: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "existing evidence\n" {
		t.Fatal("rotation replaced existing diagnostic evidence")
	}
}

func TestAppendingDoesNotExtendSegmentRetention(t *testing.T) {
	now := time.Now().UTC()
	config := DiskConfig{Directory: filepath.Join(t.TempDir(), "events"), OwnerUID: os.Geteuid(), Retention: time.Hour, Clock: func() time.Time { return now }}
	recorder, err := NewRecorder(config)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	for _, delta := range []time.Duration{0, 45 * time.Minute, 30 * time.Minute} {
		now = now.Add(delta)
		if err := recorder.Record("process", "retention_check", "info", nil); err != nil {
			t.Fatal(err)
		}
		if err := recorder.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		segments, _, err := recorder.disk.segments()
		if err != nil || len(segments) != 1 {
			t.Fatalf("segment retention state: count=%d err=%v", len(segments), err)
		}
		if err := os.Chtimes(segments[0].path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	data, err := recorder.ReadDisk(t.Context(), DefaultMaximumBytes)
	if err != nil || bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatal("appended segment extended expired record retention")
	}
}
