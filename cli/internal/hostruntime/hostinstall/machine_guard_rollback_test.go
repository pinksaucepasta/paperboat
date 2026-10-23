package hostinstall

import (
	"context"
	"errors"
	"testing"
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
