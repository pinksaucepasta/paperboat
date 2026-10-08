//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSuppliedOperationLockExcludesConcurrencyAndRejectsSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.lock")
	first, err := LockSuppliedOperation(path, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := LockSuppliedOperation(path, os.Geteuid()); err == nil {
		second.Close()
		t.Fatal("concurrent installation acquired lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := LockSuppliedOperation(path, os.Geteuid())
	if err != nil {
		t.Fatal("exited caller left stale lock", err)
	}
	second.Close()
	link := path + ".symlink"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if lock, err := LockSuppliedOperation(link, os.Geteuid()); err == nil {
		lock.Close()
		t.Fatal("followed lock symlink")
	}
}

func TestSuppliedReplacementActivatesSupervisorAndWorker(t *testing.T) {
	// Native Start leaves an existing process unchanged. The supervisor owns
	// runtime initialization and launches its worker from the published image.
	disk, supervisor, worker := "old", "old", "old"
	start := func() {
		if supervisor == "" {
			supervisor, worker = disk, disk
		}
	}
	err := replaceEnrolledBinary(context.Background(),
		func(context.Context) error { supervisor, worker = "", ""; return nil },
		func(context.Context) error { disk = "new"; start(); return nil },
		func(context.Context) error { t.Fatal("unexpected recovery"); return nil },
	)
	if err != nil || supervisor != "new" || worker != "new" {
		t.Fatalf("replacement = supervisor %q worker %q: %v", supervisor, worker, err)
	}
}

func TestSuppliedReplacementRecoversAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	disk, running := "old", true
	failed := errors.New("new service failed readiness")
	err := replaceEnrolledBinary(ctx,
		func(context.Context) error { running = false; return nil },
		func(context.Context) error {
			disk = "new"
			// Install's binary journal restores the prior bytes before returning.
			disk = "old"
			cancel()
			return failed
		},
		func(recovery context.Context) error {
			if recovery.Err() != nil {
				t.Fatal("recovery inherited cancellation")
			}
			if _, ok := recovery.Deadline(); !ok {
				t.Fatal("unbounded recovery")
			}
			if disk != "old" {
				t.Fatal("restored service before binary rollback")
			}
			running = true
			return nil
		},
	)
	if !errors.Is(err, failed) || !running {
		t.Fatalf("recovery result=%v running=%v", err, running)
	}
}

func TestSuppliedReplacementStopFailureRecoversWithoutPublishing(t *testing.T) {
	failed := errors.New("stop journal persistence failed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running := true
	err := replaceEnrolledBinary(ctx,
		func(context.Context) error { running = false; cancel(); return failed },
		func(context.Context) error { t.Fatal("published after failed stop"); return nil },
		func(recovery context.Context) error {
			if recovery.Err() != nil {
				t.Fatal("recovery inherited cancellation")
			}
			if _, ok := recovery.Deadline(); !ok {
				t.Fatal("unbounded recovery")
			}
			running = true
			return nil
		},
	)
	if !errors.Is(err, failed) || !running {
		t.Fatalf("err=%v running=%v", err, running)
	}
}
