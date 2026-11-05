//go:build !windows

package diagnostics

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A busy process on a loaded host (per-packet events, ~100 a second, a disk whose fsync takes tens of milliseconds)
// must not keep other pb commands out of the ring: each command opens it and takes the writer lock, and waited
// diskOperationTimeout for it ("Local diagnostics could not be saved"; the same starvation slowed pb exec by seconds).
func TestOpeningTheRingWhileAnotherProcessWritesBusilyOnASlowDisk(t *testing.T) {
	var syncs atomic.Int64
	previous := syncDiagnosticFile
	syncDiagnosticFile = func(file *os.File) error {
		syncs.Add(1)
		time.Sleep(30 * time.Millisecond)
		return previous(file)
	}
	defer func() { syncDiagnosticFile = previous }()
	directory := filepath.Join(t.TempDir(), "diagnostics")
	writer, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for tick := time.NewTicker(10 * time.Millisecond); ; {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = writer.Record(testEvent(t, "burst"))
			}
		}
	}()
	time.Sleep(500 * time.Millisecond) // the writer is at its steady rate
	started := time.Now()
	opener, err := NewDiskRing(DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	elapsed := time.Since(started)
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("a command could not open the ring beside a busy writer after %v: %v", elapsed, err)
	}
	defer opener.Close()
	if elapsed > 500*time.Millisecond {
		t.Fatalf("opening the ring waited %v for the busy writer's lock", elapsed)
	}
	// 100 records a second for ~0.6 s: one sync per linger, not one per record.
	if got := syncs.Load(); got > 12 {
		t.Fatalf("%d syncs for about 60 records, want one per batch", got)
	}
}
