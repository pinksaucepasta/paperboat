package hostinstall

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

type recordingGuardUndo struct{ restored, discarded int }

func (u *recordingGuardUndo) Restore(context.Context) error { u.restored++; return nil }
func (u *recordingGuardUndo) Discard()                      { u.discarded++ }

// A failed update puts back the machine guard it replaced; a committed one keeps the new guard. The
// restore runs even when the caller's context is already canceled (Lane A: a failed update to 2026.10.23.6
// rolled the daemon back but left the new guard installed).
func TestAFailedUpdateRestoresTheMachineGuard(t *testing.T) {
	failed := &recordingGuardUndo{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := finishMachineGuard(canceled, failed, errors.New("the new version did not become ready")); err != nil || failed.restored != 1 || failed.discarded != 0 {
		t.Fatalf("failed update: err=%v restored=%d discarded=%d", err, failed.restored, failed.discarded)
	}
	committed := &recordingGuardUndo{}
	if err := finishMachineGuard(context.Background(), committed, nil); err != nil || committed.restored != 0 || committed.discarded != 1 {
		t.Fatalf("committed update: err=%v restored=%d discarded=%d", err, committed.restored, committed.discarded)
	}
	if err := finishMachineGuard(context.Background(), nil, errors.New("failed")); err != nil {
		t.Fatalf("an update that kept the existing guard has nothing to restore: %v", err)
	}
}

// A ready guard on another version than the build being installed is replaced: the guard and the
// daemon always run the same version after an install or update (older ready guards used to stay).
func TestAReadyGuardOnAnotherVersionIsReplaced(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, "dev-294d86df", "custom")
	if err != nil {
		t.Fatal(err)
	}
	useGuardDomains(t, "", "local.example.test", nil)
	oldProbe, oldPrepare, oldInstall := existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard
	t.Cleanup(func() {
		existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard = oldProbe, oldPrepare, oldInstall
	})
	running := "dev-e71c7180"
	existingMachineGuardReady = func(_ context.Context, version string) error {
		if running != version {
			return errors.New("machine guard runs another version")
		}
		return nil
	}
	prepareMachineGuardForInstall = func() error { return nil }
	installed := 0
	installMissingMachineGuard = func(context.Context, string, string) (guardUndo, error) {
		installed++
		running = "dev-294d86df"
		return &recordingGuardUndo{}, nil
	}
	request := Request{Executable: executable, Source: source}
	undo, err := installMachineGuardReversible(context.Background(), request)
	if err != nil || installed != 1 || undo == nil {
		t.Fatalf("mismatched guard: installed=%d undo=%v err=%v", installed, undo, err)
	}
	if undo, err := installMachineGuardReversible(context.Background(), request); err != nil || installed != 1 || undo != nil {
		t.Fatalf("matching guard was replaced again: installed=%d undo=%v err=%v", installed, undo, err)
	}
}
