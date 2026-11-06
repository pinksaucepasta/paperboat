//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
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
	restore := func(context.Context) error { restored++; return nil }
	for round := 0; round < 2; round++ {
		if err := recoverInterruptedInstall(context.Background(), paths, restore); err != nil {
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
	if err = recoverInterruptedInstall(context.Background(), paths, func(context.Context) error { restored = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(paths.journal); err != nil || restored {
		t.Fatalf("a live installer's journal was recovered: restored=%v err=%v", restored, err)
	}
}
