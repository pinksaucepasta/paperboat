//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

func interruptedInstall(t *testing.T) (installPaths, Request, []byte) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires protected native source custody")
	}
	request := validRequest(t)
	account, err := user.LookupId("0")
	if err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(account.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, "paperboat-interrupted-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths := installPaths{root: root, worker: filepath.Join(root, "pb"), workerNext: filepath.Join(root, "pb.next"),
		workerRollback: filepath.Join(root, "pb.rollback"), metadata: filepath.Join(root, "metadata"),
		journal: filepath.Join(root, "install-journal"), updateState: filepath.Join(root, "update")}
	if err = os.Mkdir(paths.updateState, 0700); err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(request.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths.worker, image, 0755); err != nil {
		t.Fatal(err)
	}
	request.Executable = paths.worker
	if err = writeInstallMetadata(paths.metadata, request); err != nil {
		t.Fatal(err)
	}
	// The killed install had captured the metadata and staged the next worker; nothing was activated yet.
	journal, err := newInstallJournal(paths, request.Source)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Stage != "prepared" {
		t.Fatalf("a new journal starts at %q", journal.Stage)
	}
	if err = writeJournal(paths.journal, journal); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths.workerNext, []byte("half copied"), 0755); err != nil {
		t.Fatal(err)
	}
	return paths, request, journal.PreviousMetadata
}

// An install killed before it activated anything left a "prepared" journal that the updater's readiness probe
// rejects, so the updater crash-looped with "invalid privileged installation request". Recovery undoes the install
// once and the probe then reads the installed version again.
func TestInterruptedInstallIsRecoveredOnceAndTheUpdaterReadsTheInstalledSource(t *testing.T) {
	paths, request, previous := interruptedInstall(t)
	if _, _, err := pendingInstalledSource(paths, request.UID); err == nil {
		t.Fatal("the stale journal was accepted by the readiness probe")
	}
	restored := 0
	restore := func(context.Context) (bool, error) { restored++; return false, nil }
	for round := 0; round < 2; round++ {
		if _, err := recoverInterruptedInstall(context.Background(), paths, restore); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if restored != 1 {
		t.Fatalf("services were restored %d times, want once", restored)
	}
	if _, err := os.Lstat(paths.journal); !os.IsNotExist(err) {
		t.Fatalf("journal still present: %v", err)
	}
	if _, err := os.Lstat(paths.workerNext); !os.IsNotExist(err) {
		t.Fatalf("staged worker still present: %v", err)
	}
	if body, err := os.ReadFile(paths.metadata); err != nil || !bytes.Equal(body, previous) {
		t.Fatalf("metadata not back to the previous install: %v", err)
	}
	source, _, err := pendingInstalledSource(paths, request.UID)
	if err != nil || source != request.Source {
		t.Fatalf("readiness after recovery: source=%v err=%v", source == request.Source, err)
	}
}

// A journal whose installer is still running belongs to it: recovery must not roll it back.
func TestRunningInstallKeepsItsJournal(t *testing.T) {
	paths, _, _ := interruptedInstall(t)
	lock, err := updated.LockUnixNativeInstall(context.Background(), paths.updateState)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	previousWait := interruptedInstallWriterWait
	interruptedInstallWriterWait = 200 * time.Millisecond
	defer func() { interruptedInstallWriterWait = previousWait }()
	restored := false
	if _, err = recoverInterruptedInstall(context.Background(), paths, func(context.Context) (bool, error) { restored = true; return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(paths.journal); err != nil || restored {
		t.Fatalf("a live installer's journal was recovered: restored=%v err=%v", restored, err)
	}
}

// A lifecycle recovery restores the worker, then the services, and removes the journal last. Interrupting it between
// those steps leaves the journal, so the next start runs the whole recovery again; once it completes, the rollback
// copy of the worker is not left behind.
func TestInterruptedRecoveryRunsAgainFromTheTop(t *testing.T) {
	paths, request, previous := interruptedInstall(t)
	if err := os.WriteFile(paths.workerRollback, []byte("old worker"), 0755); err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(filepath.Dir(paths.root), filepath.Base(paths.root)+"-pin")
	image, err := os.ReadFile(paths.worker)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pin, image, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(pin) })
	if err = os.WriteFile(paths.worker, []byte("candidate"), 0755); err != nil { // the killed install had activated it
		t.Fatal(err)
	}
	interrupted := true
	runs, startedOnCandidate := 0, 0
	restore := func(context.Context) (bool, error) {
		runs++
		if err := os.Rename(pin, paths.worker+".next"); err != nil { // worker first: the pin is the restore source
			if !os.IsNotExist(err) {
				return false, err
			}
			if err = os.WriteFile(paths.worker+".next", image, 0755); err != nil {
				return false, err
			}
		}
		if err := os.Rename(paths.worker+".next", paths.worker); err != nil {
			return false, err
		}
		if interrupted { // the process dies after the worker is back and before the services are started
			return false, errInterruptedRecovery
		}
		if body, _ := os.ReadFile(paths.worker); !bytes.Equal(body, image) {
			startedOnCandidate++ // services would start on the wrong binary
		}
		return true, removeJournal(paths.journal)
	}
	if _, err = recoverInterruptedInstall(context.Background(), paths, restore); err != errInterruptedRecovery {
		t.Fatalf("interrupted run: %v", err)
	}
	if _, err = os.Lstat(paths.journal); err != nil {
		t.Fatalf("an interrupted recovery must keep the journal so it runs again: %v", err)
	}
	interrupted = false
	if restartSelf, err := recoverInterruptedInstall(context.Background(), paths, restore); err != nil || !restartSelf {
		t.Fatalf("second run restartSelf=%v err=%v", restartSelf, err)
	}
	if false {
		t.Fatalf("second run: %v", err)
	}
	if _, err = recoverInterruptedInstall(context.Background(), paths, restore); err != nil || runs != 2 {
		t.Fatalf("a finished recovery ran again: runs=%d err=%v", runs, err)
	}
	if startedOnCandidate != 0 {
		t.Fatal("services were started while the worker still held the candidate")
	}
	if body, _ := os.ReadFile(paths.worker); !bytes.Equal(body, image) {
		t.Fatal("the worker is not the previous binary")
	}
	for _, leftover := range []string{paths.workerRollback, paths.workerNext, paths.journal} {
		if _, err = os.Lstat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", leftover, err)
		}
	}
	_, _, _ = request, previous, pin
}

var errInterruptedRecovery = errors.New("interrupted")

// The journal is the only thing a later start can retry from. A native rollback that fails must not have removed
// it first (it was removed before the rollback, so nothing retried and every later install was refused as pending).
func TestFileRollbackKeepsTheJournalUntilTheNativeRollbackSucceeded(t *testing.T) {
	paths, _, _ := interruptedInstall(t)
	installation := unixCarrierInstallation{paths: paths}
	native := filepath.Join(paths.updateState, "native-install.json")
	if err := os.WriteFile(native, []byte("{}"), 0644); err != nil { // wrong mode: the native rollback refuses it
		t.Fatal(err)
	}
	if err := installation.finishFileRollback(); err == nil {
		t.Fatal("a refused native rollback was reported as done")
	}
	if _, err := os.Lstat(paths.journal); err != nil {
		t.Fatalf("the journal was removed although the native rollback failed: %v", err)
	}
	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	if err := installation.finishFileRollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.journal); !os.IsNotExist(err) {
		t.Fatalf("the journal outlived a finished rollback: %v", err)
	}
}
