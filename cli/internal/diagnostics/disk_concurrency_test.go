//go:build darwin || linux || windows

package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func concurrencyEvent(t *testing.T, state string) Event {
	t.Helper()
	event, err := NewEvent(time.Now().UTC(), "daemon", "state_changed", "info", map[string]string{"state": state})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestDiagnosticSubprocessWriter(t *testing.T) {
	directory := os.Getenv("PAPERBOAT_DIAGNOSTIC_WRITER_DIR")
	if directory == "" {
		t.Skip("subprocess fixture")
	}
	ring, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	for range 64 {
		if err := ring.Record(concurrencyEvent(t, "shared")); err != nil {
			t.Fatal(err)
		}
	}
	if err := ring.Close(); err != nil {
		t.Fatal(err)
	}
	if ring.Stats().DroppedRecords != 0 {
		t.Fatal("fixture dropped accepted records")
	}
}

func TestMultipleProcessesShareRetentionAndPreserveAcceptedRecords(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "diagnostics")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	commands := make([]*exec.Cmd, 2)
	for i := range commands {
		commands[i] = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDiagnosticSubprocessWriter$")
		commands[i].Env = append(os.Environ(), "PAPERBOAT_DIAGNOSTIC_WRITER_DIR="+directory)
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("shared writer: %v", err)
		}
	}
	ring, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	defer ring.Close()
	data, err := ring.ReadAll(ctx, DefaultMaximumBytes)
	if err != nil || strings.Count(string(data), "\n") != 128 {
		t.Fatalf("accepted records=%d err=%v", strings.Count(string(data), "\n"), err)
	}
}

func TestConcurrentStartupWithMaximumDiagnosticHistory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	event := concurrencyEvent(t, "shared")
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	recordsPerSegment := int(defaultSegmentBytes)/len(encoded) - 1
	segmentData := bytes.Repeat(encoded, recordsPerSegment)
	now := time.Now().UTC()
	for index := 0; index < DefaultMaximumBytes/defaultSegmentBytes; index++ {
		name := fmt.Sprintf("events-%020d-%02d.ndjson", now.Add(-time.Duration(index)*time.Second).UnixNano(), 0)
		if err := os.WriteFile(filepath.Join(directory, name), segmentData, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	const concurrentInitializers = 8
	start := make(chan struct{})
	type startupResult struct {
		ring *DiskRing
		err  error
	}
	results := make(chan startupResult, concurrentInitializers)
	for range concurrentInitializers {
		go func() {
			<-start
			ring, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
			results <- startupResult{ring: ring, err: err}
		}()
	}
	close(start)
	rings := make([]*DiskRing, 0, concurrentInitializers)
	var startupErr error
	for range concurrentInitializers {
		result := <-results
		if result.err != nil && startupErr == nil {
			startupErr = result.err
		}
		if result.ring != nil {
			rings = append(rings, result.ring)
		}
	}
	if startupErr != nil {
		for _, ring := range rings {
			_ = ring.Close()
		}
		t.Fatalf("diagnostic store startup failed under shared history contention: %T", startupErr)
	}
	t.Cleanup(func() {
		for _, ring := range rings {
			if err := ring.Close(); err != nil {
				t.Errorf("close diagnostic store: %T", err)
			}
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := rings[0].Record(event); err != nil {
		t.Fatalf("record after concurrent startup: %T", err)
	}
	if err := rings[0].Flush(ctx); err != nil {
		t.Fatalf("flush after concurrent startup: %T", err)
	}
	data, err := rings[0].ReadAll(ctx, DefaultMaximumBytes)
	wantRecords := (DefaultMaximumBytes/defaultSegmentBytes)*recordsPerSegment + 1
	if err != nil || bytes.Count(data, []byte{'\n'}) != wantRecords {
		t.Fatalf("history records=%d want=%d err=%T", bytes.Count(data, []byte{'\n'}), wantRecords, err)
	}
}

func TestContendedDiskShutdownAndReadAreBounded(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "diagnostics")
	ring, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireDiskLock(t.Context(), filepath.Join(directory, "writer.lock"), ring.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := ring.Record(concurrencyEvent(t, "blocked")); err != nil {
		t.Fatal(err)
	}
	read, stopRead := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, err = ring.ReadAll(read, DefaultMaximumBytes)
	stopRead()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended read: %v", err)
	}
	shutdown, stopShutdown := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err = ring.CloseContext(shutdown)
	stopShutdown()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended close: %v", err)
	}
	select {
	case <-ring.done:
	case <-time.After(time.Second):
		t.Fatal("canceled disk worker leaked")
	}
}

func TestDiagnosticLockRejectsSymlinkAndForeignPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions; Windows ACL/reparse validation has native tests")
	}
	owner, err := resolveDiagnosticOwner(DiskConfig{OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "writer.lock")
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if unlock, err := acquireDiskLock(t.Context(), path, owner); err == nil {
		unlock()
		t.Fatal("symlink lock accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if unlock, err := acquireDiskLock(t.Context(), path, owner); !errors.Is(err, ErrInvalid) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("permissive lock: %v", err)
	}
}

func TestSaturatedDiskFlushDoesNotBlockProducersOrShutdown(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "diagnostics")
	ring, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid(), QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer ring.Close()
	unlock, err := acquireDiskLock(t.Context(), filepath.Join(directory, "writer.lock"), ring.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := ring.Record(concurrencyEvent(t, "blocked")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(ring.queue) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(ring.queue) != 0 {
		t.Fatal("writer did not take first record")
	}
	if err := ring.Record(concurrencyEvent(t, "queued")); err != nil {
		t.Fatal(err)
	}
	flushCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	flushed := make(chan error, 1)
	go func() { flushed <- ring.Flush(flushCtx) }()
	time.Sleep(10 * time.Millisecond)
	produced := make(chan error, 1)
	event := concurrencyEvent(t, "overflow")
	go func() { produced <- ring.Record(event) }()
	select {
	case err := <-produced:
		if err != nil || ring.Stats().DroppedRecords != 1 {
			t.Fatalf("saturated producer err=%v stats=%+v", err, ring.Stats())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("flush blocked the producer")
	}
	shutdown, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stop()
	if err := ring.CloseContext(shutdown); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended shutdown=%v", err)
	}
	select {
	case <-flushed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("flush did not release after shutdown")
	}
	select {
	case <-ring.done:
	case <-time.After(time.Second):
		t.Fatal("canceled writer leaked")
	}
}
