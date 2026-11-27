//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
)

// rollback-supplied holds the activation lock for its whole run and then stops and restores the prior runtime
// (Stop, RestorePriorNativeRuntime -> Repair). Those took the lock again on a new descriptor of the same file, which
// flock refuses even inside one process: the rollback waited out its 45 s recovery context and never restored.
func TestNestedStepsOfAHeldActivationLockDoNotWaitForThemselves(t *testing.T) {
	paths, _ := heldActivation(t) // this process holds the lock, as SuppliedBinary's nativeLock does
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := lockInstallStateUnlessHeld(ctx, paths.updateState, false); !errors.Is(err, updated.ErrActivationPending) {
		t.Fatalf("re-locking a lock this process holds = %v, want it to wait and fail (the self-deadlock)", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	lock, err := lockInstallStateUnlessHeld(ctx, paths.updateState, true)
	if err != nil || lock == nil || time.Since(started) > 100*time.Millisecond {
		t.Fatalf("held lock = %v %v after %s, want admission at once", lock, err, time.Since(started))
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing the borrowed lock must not release the holder's.
	if _, err := updated.TryLockUnixNativeInstall(paths.updateState); !errors.Is(err, updated.ErrActivationPending) {
		t.Fatalf("after the nested step, the holder's lock = %v, want still held", err)
	}
}

// The bound rollback-supplied path itself hands its held lock to both nested steps (stop, then restore after the
// files are put back): calling Stop/RestorePriorNativeRuntime there waited for its own lock.
func TestBoundRollbackSuppliedPassesItsHeldLockToStopAndRestore(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root: the install journal must be root-owned")
	}
	root := t.TempDir()
	paths := installPaths{journal: filepath.Join(root, "state", "journal.json"), metadata: filepath.Join(root, "metadata.json"),
		worker: filepath.Join(root, "pb"), workerNext: filepath.Join(root, "pb.next"), workerRollback: filepath.Join(root, "pb.rollback")}
	if err := writeJournal(paths.journal, installJournal{Schema: journalSchemaV1, Stage: "services_started"}); err != nil {
		t.Fatal(err)
	}
	previousStop, previousRestore := rollbackStop, rollbackRestore
	t.Cleanup(func() { rollbackStop, rollbackRestore = previousStop, previousRestore })
	var steps []string
	rollbackStop = func(_ context.Context, _ Request, held bool) error {
		steps = append(steps, "stop held="+strconv.FormatBool(held))
		return nil
	}
	rollbackRestore = func(_ context.Context, _ installPaths, _ int, held bool) error {
		steps = append(steps, "restore held="+strconv.FormatBool(held))
		return nil
	}
	if err := rollbackSupplied(context.Background(), paths, Request{}, true, 0); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop held=true", "restore held=true"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("bound rollback steps = %v, want %v", steps, want)
	}
}
