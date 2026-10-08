//go:build darwin || linux

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type longUnixStabilityGate struct{ done <-chan struct{} }

func (g longUnixStabilityGate) HandleUpdateGate(ctx context.Context, r hostdproto.UpdateGateRequest) (hostdproto.UpdateGateResponse, error) {
	if r.Operation == hostdproto.UpdateGateStability {
		timer := time.NewTimer(time.Duration(r.WindowMillis) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return hostdproto.UpdateGateResponse{}, ctx.Err()
		case <-g.done:
			return hostdproto.UpdateGateResponse{}, context.Canceled
		}
	}
	return hostdproto.UpdateGateResponse{Target: hostdproto.UpdateGateTargetBinding{Scope: hostdproto.UpdateGateScopeStandalone, MachineID: "machine_test", FailureDomain: "standalone"}}, nil
}

// The production default gate must allow the signed observation window over
// authenticated IPC rather than truncating it at the old 35-second deadline.
func TestUnixDefaultGateAllowsSignedStabilityBeyondShortRPCDeadline(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root-owned updater storage")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	socket := filepath.Join(root, "hostd.sock")
	token := []byte(strings.Repeat("x", 32))
	server, err := hostdproto.NewServer(hostdproto.SocketConfig{SocketPath: socket, StatePath: filepath.Join(root, "fence.json"), UID: 0, GID: 0, Token: token, APIMin: 1, APIMax: 1, RequestTimeout: 31 * time.Minute, UpdateGate: longUnixStabilityGate{ctx.Done()}})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case e := <-stopped:
			if e != nil && !errors.Is(e, context.Canceled) {
				t.Error(e)
			}
		case <-time.After(time.Second):
			t.Error("IPC server did not stop")
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, e := os.Stat(socket); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("IPC listener not ready")
		}
		time.Sleep(time.Millisecond)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pb"), body, 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	previous := workerupdate.Release{Version: "2026.10.08.32", SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body)), Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1}
	if err := os.Mkdir(filepath.Join(root, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{StateRoot: filepath.Join(root, "update"), Binary: filepath.Join(root, "pb"), BinaryRollback: filepath.Join(root, "pb.rollback"), BinaryStaged: filepath.Join(root, "pb.next"), Active: previous, WorkerUID: 0, WorkerGID: 0, SocketPath: socket, Token: token, RepositoryURL: "https://example.invalid/tuf", MachineID: "machine_test", Health: HTTPHealth{Endpoint: "http://127.0.0.1:1/healthz"}, ControlSocket: filepath.Join(root, "control.sock"), ActivationController: &controllerFixture{}, Participants: &participantFixture{}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := previous
	candidate.Version = "2026.10.08.33"
	candidate.ManifestSHA256 = strings.Repeat("b", 64)
	candidate.CanaryPath = "/healthz"
	candidate.CanaryStatus = 200
	candidate.CanarySamples = 2
	candidate.CanaryTimeout = time.Second
	candidate.DrainTimeout = time.Second
	candidate.StabilityWindow = 36 * time.Second
	candidate.StabilityInterval = time.Second
	candidate.RollbackTimeout = time.Second
	err = s.config.ActivationGate.Active(ctx, workerupdate.GateRequest{TransactionID: "txn-long-stability", Previous: previous, Candidate: candidate, Worker: hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-candidate", APIVersion: 1, Epoch: 2}, Window: candidate.StabilityWindow, Interval: candidate.StabilityInterval})
	if err != nil {
		t.Fatalf("signed stability truncated by default RPC deadline: %v", err)
	}
}
