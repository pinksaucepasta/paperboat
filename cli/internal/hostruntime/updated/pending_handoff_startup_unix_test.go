//go:build darwin || linux

package updated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// A helper owns monitoring across RestartUpdater. The replacement updater must
// expose its executing identity without recovering the helper's transaction.
func TestUnixPendingHandoffStartsCandidateControlWithoutRecoveringJournal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root-owned updater storage")
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
	candidate := workerupdate.Release{Version: buildinfo.Version, SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body)), Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1}
	previous := candidate
	previous.Version = "2026.10.08.20"
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "pb")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	controlRoot := filepath.Join(root, "control")
	if err := os.Mkdir(controlRoot, 0755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(root, "pb.next")
	journal := updateflow.Journal{
		Schema: updateflow.SchemaV1, TransactionID: "txn-pending-startup", Stage: updateflow.StageMonitoring,
		ActiveVersion: previous.Version, ActiveDigest: previous.SHA256, ActiveLength: previous.Length,
		ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1,
		CandidateVersion: candidate.Version, CandidateDigest: candidate.SHA256, CandidateLength: candidate.Length,
		StagedPath: staged, HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1,
		WorkerID: "runtime-candidate", WorkerEpoch: 2, BootID: "hostd",
		StageUpdatedAt: time.Now().UTC(), HealthDeadline: time.Now().Add(time.Minute).UTC(),
	}
	journalPath := filepath.Join(root, "transaction.json")
	if err := updateflow.Write(journalPath, journal, 0, 0); err != nil {
		t.Fatal(err)
	}
	handoff := &unixActivationHandoff{Schema: unixHandoffSchema, Previous: previous, Candidate: candidate.Version, ApprovalID: strings.Repeat("a", 64), Manual: true, Started: true}
	if err := writeUnixHandoff(root, handoff); err != nil {
		t.Fatal(err)
	}
	lock, err := unixActivationLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	active, pending, err := UnixActivationActive(root)
	if err != nil || !pending || active.Version != previous.Version {
		t.Fatalf("pending startup identity=%+v pending=%v error=%v", active, pending, err)
	}
	originalJournal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	originalHandoff, err := os.ReadFile(unixHandoffPath(root))
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(controlRoot, "control.sock")
	controller := &controllerFixture{}
	s, err := New(Config{StateRoot: root, Binary: binary, BinaryRollback: filepath.Join(root, "pb.rollback"), BinaryStaged: staged, Active: candidate, WorkerUID: 0, WorkerGID: 0, SocketPath: filepath.Join(root, "hostd.sock"), Token: make([]byte, 32), RepositoryURL: "https://example.invalid/tuf", MachineID: "machine_test", Health: HTTPHealth{Endpoint: "http://127.0.0.1:1/healthz"}, ActivationGate: &gateFixture{}, ControlSocket: socket, ActivationController: controller, Participants: &participantFixture{}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reachedReady := errors.New("test readiness reached")
	ready := false
	err = s.RunWithReady(ctx, func() error {
		response, err := client.Status(ctx)
		if err != nil {
			t.Fatalf("candidate READY control inaccessible: %v", err)
		}
		if response.UpdaterVersion != candidate.Version || response.Version != previous.Version || response.Transaction.Stage != updateflow.StageMonitoring {
			t.Fatalf("candidate READY identity: updater=%q logical=%q stage=%q", response.UpdaterVersion, response.Version, response.Transaction.Stage)
		}
		ready = true
		return reachedReady
	})
	if !ready || !errors.Is(err, reachedReady) {
		t.Fatalf("pending helper prevented candidate readiness: ready=%v error=%v", ready, err)
	}
	for path, expected := range map[string][]byte{journalPath: originalJournal, unixHandoffPath(root): originalHandoff} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("candidate startup changed helper-owned %s: %v", filepath.Base(path), err)
		}
	}
	if controller.restarts != 0 {
		t.Fatalf("candidate startup restarted updater %d times", controller.restarts)
	}
}
