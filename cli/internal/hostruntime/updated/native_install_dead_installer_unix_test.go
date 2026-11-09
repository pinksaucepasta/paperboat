//go:build darwin || linux

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// An installer killed after it wrote native-install.json leaves the marker with no process holding the
// activation lock. The updater that finds it under the lock restores the pre-install transaction; before,
// it served control with the marker in place and every activation or install refused forever.
func TestUnixRunRollsBackMarkerOfDeadInstaller(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root-owned updater storage")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	originalVersion := buildinfo.Version
	buildinfo.Version = "2026.10.08.30"
	defer func() { buildinfo.Version = originalVersion }()
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
	idle := updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "txn-before-install", Stage: updateflow.StageIdle, StageUpdatedAt: time.Now().UTC(), BootID: "hostd", ActiveVersion: active.Version, ActiveDigest: active.SHA256, ActiveLength: active.Length, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1}
	if err := updateflow.Write(journalPath, idle, 0, 0); err != nil {
		t.Fatal(err)
	}
	lock, err := beginNativeInstallFixture(root) // the installer: marker written, transaction.json removed
	if err != nil {
		t.Fatal(err)
	}
	lock.Close() // ...and it dies
	if pending, err := nativeInstallPending(root); err != nil || !pending {
		t.Fatalf("fixture marker pending=%v err=%v", pending, err)
	}
	controlRoot := filepath.Join(root, "control")
	if err := os.Mkdir(controlRoot, 0755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(controlRoot, "control.sock")
	s, err := New(nativeFixtureConfig(Config{StateRoot: root, Binary: binary, BinaryRollback: filepath.Join(root, "pb.rollback"), BinaryStaged: filepath.Join(root, "pb.next"), Active: active, WorkerUID: 0, WorkerGID: 0, SocketPath: filepath.Join(root, "hostd.sock"), Token: make([]byte, 32), RepositoryURL: "https://example.invalid/tuf", MachineID: "machine_test", Health: HTTPHealth{Endpoint: "http://127.0.0.1:1/healthz"}, ActivationGate: &gateFixture{}, ControlSocket: socket, ActivationController: &controllerFixture{}, Participants: &participantFixture{}}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reached := errors.New("ready")
	err = s.RunWithReady(ctx, func() error {
		if pending, err := nativeInstallPending(root); err != nil || pending {
			t.Errorf("marker survived startup pending=%v err=%v", pending, err)
		}
		if got, err := updateflow.Load(journalPath); err != nil || got.TransactionID != idle.TransactionID {
			t.Errorf("pre-install transaction not restored: %+v %v", got, err)
		}
		return reached
	})
	if !errors.Is(err, reached) {
		t.Fatalf("run: %v", err)
	}
}
