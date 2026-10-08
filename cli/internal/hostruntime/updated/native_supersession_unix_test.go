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

func TestNativePackageStartupSupersedesOnlyOlderPreCutoverJournal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native updater startup requires protected root-owned storage")
	}
	previousVersion := buildinfo.Version
	buildinfo.Version = "2026.09.07.958"
	defer func() { buildinfo.Version = previousVersion }()
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
	for _, tc := range []struct {
		name, version string
		stage         updateflow.Stage
		ready         bool
	}{
		{"older idle", "2026.09.07.957", updateflow.StageIdle, true},
		{"older postcutover", "2026.09.07.957", updateflow.StageCutover, false},
		{"newer idle", "2026.09.07.959", updateflow.StageIdle, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := os.MkdirTemp("", "pb-native-startup-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			controlRoot := filepath.Join(root, "control")
			if err := os.Mkdir(controlRoot, 0755); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(root, "pb")
			if err := os.WriteFile(binary, body, 0700); err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(root, "pb.next")
			journal := updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "txn-native-install", Stage: tc.stage, ActiveVersion: tc.version, ActiveDigest: active.SHA256, ActiveLength: active.Length, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1, BootID: "hostd", StageUpdatedAt: time.Now().UTC()}
			if tc.stage == updateflow.StageCutover {
				journal.CandidateVersion = active.Version
				journal.CandidateDigest = active.SHA256
				journal.CandidateLength = active.Length
				journal.StagedPath = staged
				journal.WorkerID = "runtime-candidate"
				journal.WorkerEpoch = 2
			}
			path := filepath.Join(root, "transaction.json")
			if err := updateflow.Write(path, journal, 0, 0); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(Config{StateRoot: root, Binary: binary, BinaryRollback: filepath.Join(root, "pb.rollback"), BinaryStaged: staged, Active: active, WorkerUID: 0, WorkerGID: 0, SocketPath: filepath.Join(root, "hostd.sock"), Token: make([]byte, 32), RepositoryURL: "https://example.invalid/tuf", MachineID: "machine_test", Health: HTTPHealth{Endpoint: "http://127.0.0.1:1/healthz"}, ActivationGate: &gateFixture{}, ControlSocket: filepath.Join(controlRoot, "control.sock"), ActivationController: &controllerFixture{}, Participants: &participantFixture{}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reachedReady := errors.New("test readiness reached")
			err = s.RunWithReady(ctx, func() error { return reachedReady })
			if tc.ready {
				if !errors.Is(err, reachedReady) {
					t.Fatalf("new native package never became ready: %v", err)
				}
				got, err := updateflow.Load(path)
				if err != nil || got.Stage != updateflow.StageIdle || got.ActiveVersion != active.Version || got.ActiveDigest != active.SHA256 {
					t.Fatalf("superseded journal=%+v error=%v", got, err)
				}
			} else {
				if err == nil || errors.Is(err, reachedReady) {
					t.Fatalf("unsafe supersession reached readiness: %v", err)
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != string(original) {
					t.Fatalf("rejected transaction changed: %v", readErr)
				}
			}
		})
	}
}
