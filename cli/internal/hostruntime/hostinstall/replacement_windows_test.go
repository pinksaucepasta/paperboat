//go:build windows

package hostinstall

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsReplacementRejectsMalformedNativeSnapshotBeforeMutation(t *testing.T) {
	layout, err := service.WindowsUserLayout("S-1-5-21-111-222-333-1001")
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []string{"", "null", `{}`, `{"unknown":true}`, `{} {}`, `{"installation_absent":false}`, `{"installation_absent":true,"source":{}}`, `{"installation_absent":false,"config":{},"source":{}}`, strings.Repeat("x", (128<<10)+1)} {
		if err := (windowsInstallationState{layout: layout}).Restore(context.Background(), []byte(snapshot)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid snapshot error = %v", err)
		}
	}
}

// First enrollment journals an absent baseline: no predecessor, never a corrupt record. The updater service read
// it as ErrInvalidRequest and exited, so enrollment on Windows failed at "install services".
func TestWindowsCapturedSourceOfAnAbsentInstallationIsNotExist(t *testing.T) {
	body, err := json.Marshal(windowsInstallationSnapshot{InstallationAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := windowsCapturedSourceProjection(body, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent baseline error = %v, want os.ErrNotExist", err)
	}
	for _, corrupt := range []string{"", "{}", `{"installation_absent":false}`, `{"installation_absent":true,"source":{}}`, "x"} {
		if _, err := windowsCapturedSourceProjection([]byte(corrupt), ""); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("corrupt snapshot %q error = %v, want ErrInvalidRequest", corrupt, err)
		}
	}
}

// victus: after a failed update left hostd stopped, `pb install` of a good build failed with "hostd lifecycle pipe
// connection failed" and could never recover the machine. A stopped hostd has nothing to admit.
func TestWindowsReplacementSkipsMaintenanceAdmissionWhenHostdIsStopped(t *testing.T) {
	layout, err := service.WindowsUserLayout("S-1-5-21-111-222-333-1001")
	if err != nil {
		t.Fatal(err)
	}
	restore := windowsHostdRunning
	t.Cleanup(func() { windowsHostdRunning = restore })
	windowsHostdRunning = func(context.Context, service.Layout) (bool, error) { return false, nil }
	abort, err := prepareWindowsReplacementMaintenance(context.Background(), layout, `C:\does-not-exist\hostd.token`, "", false, time.Second)
	if err != nil {
		t.Fatalf("stopped hostd still required maintenance admission: %v", err)
	}
	if err := abort(); err != nil {
		t.Fatal(err)
	}
	windowsHostdRunning = func(context.Context, service.Layout) (bool, error) { return true, nil }
	if _, err := prepareWindowsReplacementMaintenance(context.Background(), layout, `C:\does-not-exist\hostd.token`, "", false, time.Second); err == nil {
		t.Fatal("a running hostd was not asked for maintenance admission")
	}
}

// victus 2026-11-10: after a failed update the restored hostd exited with "terminal owner unavailable": nothing
// started the previous version's session owner again. The installation restore, which the lifecycle runs before it
// restores any service, prepares the restored record's own owner from the restored executable.
func TestWindowsRestoreStartsThePreviousSessionOwner(t *testing.T) {
	layout, err := service.WindowsUserLayout("S-1-5-21-111-222-333-1001")
	if err != nil {
		t.Fatal(err)
	}
	old := prepareRestoredSessionOwner
	t.Cleanup(func() { prepareRestoredSessionOwner = old })
	var prepared []Request
	unavailable := errors.New("session owner did not become ready")
	var prepareErr error
	prepareRestoredSessionOwner = func(_ context.Context, request Request) error {
		prepared = append(prepared, request)
		return prepareErr
	}
	config := WindowsRuntimeConfig{OwnerSID: "S-1-5-21-111-222-333-1001", StateRoot: `C:\Users\dev\AppData\Local\Paperboat`, Workspace: `C:\Users\dev`, ListenAddress: "127.0.0.1:7777"}
	config.Source.SHA256 = strings.Repeat("ab", 32)
	// A record without a session owner (awaiting enrollment) has none to start.
	if err := restoreWindowsSessionOwner(context.Background(), config, layout); err != nil || len(prepared) != 0 {
		t.Fatalf("record without a session owner: err=%v prepared=%d", err, len(prepared))
	}
	config.SessionOwnerGeneration = config.Source.SHA256[:32]
	if err := restoreWindowsSessionOwner(context.Background(), config, layout); err != nil || len(prepared) != 1 {
		t.Fatalf("restored record: err=%v prepared=%d", err, len(prepared))
	}
	got := prepared[0]
	if got.Source != config.Source || got.SessionOwnerGeneration != config.SessionOwnerGeneration || got.Executable != layout.Binary ||
		got.OwnerSID != config.OwnerSID || got.StateRoot != config.StateRoot || got.WorkspaceRoot != config.Workspace {
		t.Fatalf("session owner prepared for another installation than the restored one: %+v", got)
	}
	prepareErr = unavailable
	if err := restoreWindowsSessionOwner(context.Background(), config, layout); !errors.Is(err, unavailable) {
		t.Fatalf("a session owner that did not start must fail the restore, got %v", err)
	}
}

// victus 2026-11-10: an update killed after it stopped the services was recovered only by `pb doctor --repair`,
// and its session owner stayed running: nothing recorded which version the dead installer was installing. The
// record names it; the recovery withdraws it once and a failed withdrawal keeps the record for the next look.
func TestWindowsRecordedCandidateIsWithdrawnOnceAfterItsInstallerDied(t *testing.T) {
	previousRoot, previousWithdraw := windowsProgramDataRoot, withdrawRecordedCandidate
	t.Cleanup(func() { windowsProgramDataRoot, withdrawRecordedCandidate = previousRoot, previousWithdraw })
	windowsProgramDataRoot = filepath.Join(t.TempDir(), "Paperboat")
	const ownerSID = "S-1-5-21-111-222-333-1001"
	record, err := windowsCandidateRecordPath(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		t.Fatal(err)
	}
	var withdrawn []candidateSessionOwner
	var withdrawErr error
	withdrawRecordedCandidate = func(_ context.Context, request Request, candidate candidateSessionOwner) error {
		if request.OwnerSID != ownerSID {
			t.Fatalf("withdrawn for another account: %q", request.OwnerSID)
		}
		withdrawn = append(withdrawn, candidate)
		return withdrawErr
	}
	// No replacement was interrupted: nothing to recover, nothing withdrawn.
	if pending, err := WindowsInstallInterrupted(ownerSID); err != nil || pending {
		t.Fatalf("pending=%v err=%v without a record", pending, err)
	}
	if err := withdrawRecordedWindowsCandidate(context.Background(), ownerSID); err != nil || len(withdrawn) != 0 {
		t.Fatalf("err=%v withdrawn=%d without a record", err, len(withdrawn))
	}
	source := installsource.Source{Version: "dev-candidate", Platform: "windows", Architecture: "amd64", SHA256: strings.Repeat("cd", 32), Length: 1, Distribution: installsource.Custom}
	write := func(body []byte) {
		t.Helper()
		if err := os.WriteFile(record, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(windowsCandidateRecord{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	write(body)
	if pending, err := WindowsInstallInterrupted(ownerSID); err != nil || !pending {
		t.Fatalf("pending=%v err=%v with a record", pending, err)
	}
	withdrawErr = errors.New("the candidate's session owner service did not stop")
	if err := withdrawRecordedWindowsCandidate(context.Background(), ownerSID); !errors.Is(err, withdrawErr) {
		t.Fatalf("failed withdrawal error = %v", err)
	}
	if pending, _ := WindowsInstallInterrupted(ownerSID); !pending {
		t.Fatal("a failed withdrawal removed the record: the candidate would stay for good")
	}
	withdrawErr = nil
	if err := withdrawRecordedWindowsCandidate(context.Background(), ownerSID); err != nil {
		t.Fatal(err)
	}
	if len(withdrawn) != 2 || withdrawn[1].generation != source.SHA256[:32] || withdrawn[1].existed {
		t.Fatalf("withdrawn %+v", withdrawn)
	}
	if pending, _ := WindowsInstallInterrupted(ownerSID); pending {
		t.Fatal("the record stayed after its candidate was withdrawn")
	}
	// A session owner that was published before the attempt is recorded as such and is not this attempt's.
	body, _ = json.Marshal(windowsCandidateRecord{Source: source, Existed: true})
	write(body)
	if err := withdrawRecordedWindowsCandidate(context.Background(), ownerSID); err != nil || !withdrawn[2].existed {
		t.Fatalf("err=%v withdrawn=%+v", err, withdrawn)
	}
	// A record that cannot be read names no version: nothing is withdrawn on a guess.
	write([]byte(`{"source":{}}`))
	if err := withdrawRecordedWindowsCandidate(context.Background(), ownerSID); !errors.Is(err, ErrInvalidRequest) || len(withdrawn) != 3 {
		t.Fatalf("corrupt record: err=%v withdrawn=%d", err, len(withdrawn))
	}
}
