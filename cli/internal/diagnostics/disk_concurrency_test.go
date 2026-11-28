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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// Reading 50 MiB of history inside the production bound is a speed test; this one checks that
	// concurrent startup keeps every record, so give the operations time under -race on a busy host.
	previous := diskOperationTimeout
	diskOperationTimeout = 60 * time.Second
	t.Cleanup(func() { diskOperationTimeout = previous })
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
	owner, err := resolveDiagnosticOwner(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < DefaultMaximumBytes/defaultSegmentBytes; index++ {
		name := fmt.Sprintf("events-%020d-%02d.ndjson", now.Add(-time.Duration(index)*time.Second).UnixNano(), 0)
		// Written as the store writes its own files: mode 0600, and the store's protected ACL on Windows.
		if err := writeDiagnosticAtomic(filepath.Join(directory, name), segmentData, owner); err != nil {
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

	// Reading the maximum history is bounded only as a hang guard; under the race detector on a busy
	// machine it takes several seconds.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
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

// Two processes at exit on a slow disk: one writer's sync stalls past every save bound, and a second must still open
// the ring, append and finish its own shutdown. A sync under writer.lock made the second one lose its records.
func TestASiblingsStalledDiskSyncDoesNotStopAnotherWritersShutdown(t *testing.T) {
	previous := syncDiagnosticFile
	var first atomic.Bool
	stalled, resume := make(chan struct{}), make(chan struct{})
	syncDiagnosticFile = func(file *os.File) error {
		if first.CompareAndSwap(false, true) {
			close(stalled)
			<-resume
		}
		return previous(file)
	}
	t.Cleanup(func() { syncDiagnosticFile = previous })
	config := DiskConfig{Directory: filepath.Join(t.TempDir(), "diagnostics"), OwnerUID: os.Geteuid()}
	sibling, err := NewDiskRing(config)
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	var released sync.Once
	release := func() { released.Do(func() { close(resume) }) }
	defer release()
	if err := sibling.Record(concurrencyEvent(t, "sibling")); err != nil {
		t.Fatal(err)
	}
	<-stalled
	ring, err := NewDiskRing(config)
	if err != nil {
		t.Fatalf("open beside a stalled sync: %v", err)
	}
	if err := ring.Record(concurrencyEvent(t, "exiting")); err != nil {
		t.Fatal(err)
	}
	shutdown, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := errors.Join(ring.Flush(shutdown), ring.CloseContext(shutdown)); err != nil {
		t.Fatalf("shutdown beside a stalled sync: %v (%+v)", err, ring.Stats())
	}
	release()
	if err := sibling.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := sibling.ReadAll(t.Context(), DefaultMaximumBytes)
	if err != nil || bytes.Count(data, []byte{'\n'}) != 2 {
		t.Fatalf("want both writers' records: %q %v", data, err)
	}
}

// A ring opened while the lock stayed busy skipped recovery: its first append must still repair a crashed tail.
func TestARingOpenedBesideABusyWriterRepairsACrashedTailBeforeAppending(t *testing.T) {
	config := DiskConfig{Directory: filepath.Join(t.TempDir(), "diagnostics"), OwnerUID: os.Geteuid()}
	first, err := NewDiskRing(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(first.Record(concurrencyEvent(t, "before")), first.Close()); err != nil {
		t.Fatal(err)
	}
	segments, err := filepath.Glob(filepath.Join(config.Directory, "events-*.ndjson"))
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments: %v %v", segments, err)
	}
	file, err := os.OpenFile(segments[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"schema":"crashed`)
	if err := errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireDiskLock(t.Context(), filepath.Join(config.Directory, "writer.lock"), first.owner)
	if err != nil {
		t.Fatal(err)
	}
	previous := diskOperationTimeout
	diskOperationTimeout = 50 * time.Millisecond
	ring, err := NewDiskRing(config)
	diskOperationTimeout = previous
	if err := errors.Join(err, unlock()); err != nil {
		t.Fatalf("open beside a held lock: %v", err)
	}
	defer ring.Close()
	for name, read := range map[string]func(context.Context, int64) ([]byte, error){"ReadAll": ring.ReadAll, "ReadTail": ring.ReadTail} {
		ring.unrepaired.Store(true)
		data, err := read(t.Context(), DefaultMaximumBytes)
		if err != nil || bytes.Contains(data, []byte("crashed")) || bytes.Count(data, []byte{'\n'}) != 1 {
			t.Fatalf("%s before any append: %q %v", name, data, err)
		}
	}
	if err := errors.Join(ring.Record(concurrencyEvent(t, "after")), ring.Flush(t.Context())); err != nil {
		t.Fatal(err)
	}
	data, err := ring.ReadAll(t.Context(), DefaultMaximumBytes)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("want the records before and after the crash: %q", data)
	}
	for _, line := range lines {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("corrupt line %q: %v", line, err)
		}
	}
}

// A real storage fault seen earlier must not turn into "only slow" because the flush or close later ran out of time.
func TestAStorageFaultOutlivesATimedOutFlushAndClose(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "diagnostics")
	ring, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+".offline"); err != nil {
		t.Fatal(err)
	}
	if err := ring.Record(concurrencyEvent(t, "faulted")); err != nil {
		t.Fatal(err)
	}
	fault := ring.Flush(t.Context())
	if fault == nil || SaveDelayed(fault) {
		t.Fatalf("storage fault: %v", fault)
	}
	if err := os.Rename(directory+".offline", directory); err != nil {
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
	short, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stop()
	for name, err := range map[string]error{"flush": ring.Flush(short), "close": ring.CloseContext(short)} {
		if err == nil || SaveDelayed(err) {
			t.Errorf("timed-out %s hid the storage fault: %v", name, err)
		}
	}
	<-ring.done
}

// A sibling keeps its segment open while it syncs outside writer.lock. Windows refuses to delete such a file: expiry
// and the byte budget must leave it for a later append instead of failing this writer as if storage were broken.
func TestASegmentASiblingStillSyncsDoesNotFailAnotherWritersRemoval(t *testing.T) {
	for name, tc := range map[string]struct {
		limit int64
		size  int
		age   time.Duration
	}{
		"expired":     {limit: DefaultMaximumBytes, size: 1, age: 2 * time.Hour},
		"over budget": {limit: MaximumRecordBytes, size: MaximumRecordBytes - 1},
	} {
		t.Run(name, func(t *testing.T) {
			config := DiskConfig{Directory: filepath.Join(t.TempDir(), "diagnostics"), OwnerUID: os.Geteuid(), Retention: time.Hour, MaximumBytes: tc.limit, SegmentBytes: MaximumRecordBytes}
			ring, err := NewDiskRing(config)
			if err != nil {
				t.Fatal(err)
			}
			defer ring.Close()
			held := filepath.Join(config.Directory, fmt.Sprintf("events-%020d-00.ndjson", time.Now().Add(-time.Minute).UnixNano()))
			if err := createDiagnosticSegment(held, ring.owner); err != nil {
				t.Fatal(err)
			}
			sibling, err := openDiagnosticAppend(held, ring.owner)
			if err != nil {
				t.Fatal(err)
			}
			defer sibling.Close()
			if _, err := sibling.Write(bytes.Repeat([]byte{'\n'}, tc.size)); err != nil {
				t.Fatal(err)
			}
			modified := time.Now().Add(-tc.age)
			if err := os.Chtimes(held, modified, modified); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(ring.Record(concurrencyEvent(t, "beside")), ring.Flush(t.Context())); err != nil {
				t.Fatalf("append beside an open segment: %v (%+v)", err, ring.Stats())
			}
			// Opening runs the same removals.
			second, err := NewDiskRing(config)
			if err != nil {
				t.Fatalf("open beside an open segment: %v", err)
			}
			defer second.Close()
			if err := sibling.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(held, modified, modified); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := errors.Join(ring.Record(concurrencyEvent(t, "after")), ring.Flush(t.Context())); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(held); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("segment not removed once closed: %v", err)
			}
		})
	}
}

// A segment a sibling holds open must not stop the eviction of idle ones: the ring stays within its budget.
func TestAHeldSegmentDoesNotStopEvictionOfIdleOnes(t *testing.T) {
	config := DiskConfig{Directory: filepath.Join(t.TempDir(), "diagnostics"), OwnerUID: os.Geteuid(), MaximumBytes: 2 * MaximumRecordBytes, SegmentBytes: MaximumRecordBytes}
	ring, err := NewDiskRing(config)
	if err != nil {
		t.Fatal(err)
	}
	defer ring.Close()
	for i := range 2 {
		path := filepath.Join(config.Directory, fmt.Sprintf("events-%020d-00.ndjson", time.Now().Add(time.Duration(i-2)*time.Minute).UnixNano()))
		if err := createDiagnosticSegment(path, ring.owner); err != nil {
			t.Fatal(err)
		}
		file, err := openDiagnosticAppend(path, ring.owner)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.Write(bytes.Repeat([]byte{'\n'}, MaximumRecordBytes-10)); err != nil {
			t.Fatal(err)
		}
		// Only the oldest stays open.
		if i == 1 {
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := errors.Join(ring.Record(concurrencyEvent(t, "beside")), ring.Flush(t.Context())); err != nil {
		t.Fatalf("append beside an open segment: %v (%+v)", err, ring.Stats())
	}
	if _, err := ring.ReadAll(t.Context(), config.MaximumBytes); err != nil {
		t.Fatalf("ring over its budget with an idle segment left to evict: %v", err)
	}
}

// The failure note is created like a segment, replaces a link instead of writing through it, and never recreates a
// removed directory.
func TestFailureNoteIsOwnerOnlyNeverFollowsALinkOrRecreatesTheDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "diagnostics")
	recorder, err := NewRecorder(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	note := filepath.Join(directory, FailureNoteName)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, note); err != nil && runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	for _, text := range []string{"first\n", "second\n"} {
		if err := recorder.NoteSaveFailure(text); err != nil {
			t.Fatal(err)
		}
	}
	if kept, err := os.ReadFile(outside); err != nil || string(kept) != "keep" {
		t.Fatalf("wrote through a link: %q %v", kept, err)
	}
	if _, err := verifiedDiagnosticFile(note, recorder.disk.owner); err != nil {
		t.Fatalf("note is not owner-only: %v", err)
	}
	if got, err := os.ReadFile(note); err != nil || string(got) != "second\n" {
		t.Fatalf("note: %q %v", got, err)
	}
	// A reader holding the old note only delays the replacement, and callers do not collide.
	reader, err := os.Open(note)
	if err != nil {
		t.Fatal(err)
	}
	release := time.AfterFunc(50*time.Millisecond, func() { _ = reader.Close() })
	defer release.Stop()
	results := make(chan error, 4)
	for range cap(results) {
		go func() { results <- recorder.NoteSaveFailure("second\n") }()
	}
	for range cap(results) {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if left, err := filepath.Glob(filepath.Join(directory, "."+FailureNoteName+"*")); err != nil || len(left) != 0 {
		t.Fatalf("temporary files left: %v %v", left, err)
	}
	if err := os.Rename(directory, directory+".offline"); err != nil {
		t.Fatal(err)
	}
	if err := recorder.NoteSaveFailure("third\n"); err == nil {
		t.Fatal("wrote a note without its directory")
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory recreated: %v", err)
	}
	if err := NewMemoryRecorder().NoteSaveFailure("memory\n"); err != nil {
		t.Fatal(err)
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
	// An empty queue does not prove the writer reached the held lock: until its batch drain sees an
	// empty queue it also takes the queued record and the flush barrier. Produce until the writer is
	// stuck and a record is dropped; no record may wait on the flush.
	event := concurrencyEvent(t, "overflow")
	for attempt := 0; ring.Stats().DroppedRecords == 0; attempt++ {
		if attempt == 100 {
			t.Fatalf("a saturated queue never dropped a record: stats=%+v", ring.Stats())
		}
		produced := make(chan error, 1)
		go func() { produced <- ring.Record(event) }()
		select {
		case err := <-produced:
			if err != nil {
				t.Fatalf("saturated producer err=%v stats=%+v", err, ring.Stats())
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("flush blocked the producer")
		}
	}
	if dropped := ring.Stats().DroppedRecords; dropped != 1 {
		t.Fatalf("dropped %d records, want exactly the one that met a full queue", dropped)
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

// Native roles shut down together and retain their final diagnostics. A queued burst, even of thousands
// of events, becomes durable within the product's shutdown bound: the daemon closes its recorder with
// Close (diskOperationTimeout), and a pb process within its shared shutdown deadline. The writer drains a
// burst in bounded appends with one fsync each, so every event survives, in the order it was recorded.
func TestQueuedDiagnosticBurstDrainsWithinShutdownBudget(t *testing.T) {
	const events = 2048
	ring, err := NewDiskRing(DiskConfig{Directory: filepath.Join(t.TempDir(), "diagnostics"), OwnerUID: os.Geteuid(), QueueCapacity: 2 * events})
	if err != nil {
		t.Fatal(err)
	}
	defer ring.Close()
	unlock, err := acquireDiskLock(t.Context(), filepath.Join(ring.config.Directory, "writer.lock"), ring.owner)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < events; i++ {
		if err := ring.Record(concurrencyEvent(t, strconv.Itoa(i))); err != nil {
			unlock()
			t.Fatal(err)
		}
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if err := ring.Close(); err != nil {
		t.Fatalf("queued shutdown diagnostics did not become durable within the shutdown bound: %v", err)
	}
	stats := ring.Stats()
	if stats.DroppedRecords != 0 || stats.FailedRecords != 0 {
		t.Fatalf("lost shutdown diagnostics: %+v", stats)
	}
	data, err := ring.ReadAll(t.Context(), DefaultMaximumBytes)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	if len(lines) != events {
		t.Fatalf("persisted %d events; want %d", len(lines), events)
	}
	for i, line := range lines {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil || event.Fields["state"] != strconv.Itoa(i) {
			t.Fatalf("event %d persisted as %q (%v); want the events in the order they were recorded", i, line, err)
		}
	}
}
