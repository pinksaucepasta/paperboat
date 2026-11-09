//go:build darwin || linux

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// deadWriterFixture is an updater whose state root holds an idle pre-install transaction, a service for it, and
// the controller its activation helper is managed through.
func deadWriterFixture(t *testing.T) (root string, s *Service, idle updateflow.Journal, controller *controllerFixture) {
	t.Helper()
	root = t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	originalVersion := buildinfo.Version
	buildinfo.Version = "2026.10.08.30"
	t.Cleanup(func() { buildinfo.Version = originalVersion })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	active := workerupdate.Release{Version: buildinfo.Version, SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body)), Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1}
	binary := filepath.Join(root, "pb")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "transaction.json")
	idle = updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "txn-before-install", Stage: updateflow.StageIdle, StageUpdatedAt: time.Now().UTC(), BootID: "hostd", ActiveVersion: active.Version, ActiveDigest: active.SHA256, ActiveLength: active.Length, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1}
	if err := updateflow.Write(journalPath, idle, 0, 0); err != nil {
		t.Fatal(err)
	}
	controlRoot := filepath.Join(root, "control")
	if err := os.Mkdir(controlRoot, 0755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(controlRoot, "control.sock")
	controller = &controllerFixture{}
	s, err = New(nativeFixtureConfig(Config{StateRoot: root, Binary: binary, BinaryRollback: filepath.Join(root, "pb.rollback"), BinaryStaged: filepath.Join(root, "pb.next"), Active: active, WorkerUID: 0, WorkerGID: 0, SocketPath: filepath.Join(root, "hostd.sock"), Token: make([]byte, 32), RepositoryURL: "https://example.invalid/tuf", MachineID: "machine_test", Health: HTTPHealth{Endpoint: "http://127.0.0.1:1/healthz"}, ActivationGate: &gateFixture{}, ControlSocket: socket, ActivationController: controller, Participants: &participantFixture{}}))
	if err != nil {
		t.Fatal(err)
	}
	return root, s, idle, controller
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root-owned updater storage")
	}
}

// pbvm1 F14-03: an installer killed after it wrote native-install.json left the marker, and only an updater
// START rolled it back. A running updater does it too, once the installer's lock is free; never before.
func TestRunningUpdaterRollsBackTheMarkerOfADeadInstallerAndLeavesALiveOne(t *testing.T) {
	requireRoot(t)
	root, s, idle, _ := deadWriterFixture(t)
	lock, err := beginNativeInstallFixture(root) // the installer: marker written, transaction.json removed
	if err != nil {
		t.Fatal(err)
	}
	if acted, err := s.RecoverDeadWriters(context.Background()); err != nil || acted {
		t.Fatalf("a live installer's marker was touched: acted=%v err=%v", acted, err)
	}
	if pending, _ := nativeInstallPending(root); !pending {
		t.Fatal("the live installer's marker was removed")
	}
	lock.Close() // ...and it dies
	if acted, err := s.RecoverDeadWriters(context.Background()); err != nil || !acted {
		t.Fatalf("the dead installer's marker was not recovered: acted=%v err=%v", acted, err)
	}
	if pending, _ := nativeInstallPending(root); pending {
		t.Fatal("marker survived")
	}
	if got, err := updateflow.Load(filepath.Join(root, "transaction.json")); err != nil || got.TransactionID != idle.TransactionID {
		t.Fatalf("pre-install transaction not restored: %+v %v", got, err)
	}
	if acted, err := s.RecoverDeadWriters(context.Background()); err != nil || acted {
		t.Fatalf("a recovered state was recovered again: acted=%v err=%v", acted, err)
	}
}

// A transaction a killed check left in "checking" is reset by Manager.Recover, which ran only at start.
func TestRunningUpdaterResetsATransactionAKilledCheckLeft(t *testing.T) {
	requireRoot(t)
	root, s, idle, _ := deadWriterFixture(t)
	checking := idle
	checking.Stage = updateflow.StageChecking
	if err := updateflow.Write(filepath.Join(root, "transaction.json"), checking, 0, 0); err != nil {
		t.Fatal(err)
	}
	lock, err := unixActivationLock(root) // the check that is still running holds the lock
	if err != nil {
		t.Fatal(err)
	}
	if acted, err := s.RecoverDeadWriters(context.Background()); err != nil || acted {
		t.Fatalf("a live writer's transaction was touched: acted=%v err=%v", acted, err)
	}
	lock.Close()
	if acted, err := s.RecoverDeadWriters(context.Background()); err != nil || !acted {
		t.Fatalf("acted=%v err=%v", acted, err)
	}
	if got, err := s.currentManager().TransactionState(); err != nil || got.Stage != updateflow.StageIdle {
		t.Fatalf("state after recovery: %+v %v", got, err)
	}
}

// A cutover record is the activation helper's to recover, not the updater's: no error, no change, no restart.
func TestRunningUpdaterLeavesACutoverRecordToTheHelper(t *testing.T) {
	requireRoot(t)
	root, s, idle, _ := deadWriterFixture(t)
	cutover := idle
	cutover.Stage, cutover.NativeActivation = updateflow.StageCutover, true
	cutover.CandidateVersion, cutover.CandidateDigest, cutover.CandidateLength, cutover.StagedPath = "2026.10.08.31", strings.Repeat("a", 64), 1, filepath.Join(root, "pb.next")
	if err := updateflow.Write(filepath.Join(root, "transaction.json"), cutover, 0, 0); err != nil {
		t.Fatal(err)
	}
	if acted, err := s.RecoverDeadWriters(context.Background()); err != nil || acted {
		t.Fatalf("acted=%v err=%v", acted, err)
	}
}

// An activation whose helper died keeps its handoff with Started set; the running updater starts the helper again
// (once the helper's lock is free). A queued one is not started every tick.
func TestRunningUpdaterRestartsTheHelperOfAStartedActivationOnly(t *testing.T) {
	requireRoot(t)
	root, s, _, controller := deadWriterFixture(t)
	handoff := &unixActivationHandoff{Schema: unixHandoffSchema, ApprovalID: strings.Repeat("a", 64), Previous: s.config.Active, Candidate: "1.2.3.5", Manual: true, Baseline: UnixParticipantProbe{Running: true, State: "ready"}, RequireInventory: true}
	if err := writeUnixHandoff(root, handoff); err != nil {
		t.Fatal(err)
	}
	if acted, err := s.RecoverDeadWriters(context.Background()); err != nil || acted || controller.installs != 0 {
		t.Fatalf("a queued activation's helper was started: acted=%v err=%v installs=%d", acted, err, controller.installs)
	}
	handoff.Started = true
	if err := writeUnixHandoff(root, handoff); err != nil {
		t.Fatal(err)
	}
	live, err := unixActivationLock(root) // the helper is running
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverDeadWriters(context.Background()); err != nil || controller.installs != 0 {
		t.Fatalf("a live helper was restarted: err=%v installs=%d", err, controller.installs)
	}
	live.Close() // ...and it dies
	if _, err := s.RecoverDeadWriters(context.Background()); err != nil || controller.installs != 1 {
		t.Fatalf("the dead helper was not started again: err=%v installs=%d", err, controller.installs)
	}
	if loaded, err := readUnixHandoff(root); err != nil || loaded == nil {
		t.Fatalf("the handoff the helper finishes was removed: %v", err)
	}
}
