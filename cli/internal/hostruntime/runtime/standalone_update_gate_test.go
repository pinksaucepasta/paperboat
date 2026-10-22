//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

func standaloneGateRequest(operation string, target *hostdproto.UpdateGateTargetBinding) hostdproto.UpdateGateRequest {
	request := hostdproto.UpdateGateRequest{Operation: operation, TransactionID: "transaction_01", Version: "2026.09.02.2", ManifestSHA256: strings.Repeat("a", 64), ExpectedTarget: target}
	switch operation {
	case hostdproto.UpdateGateCandidate:
		request.Path, request.ExpectedStatus, request.Samples, request.TimeoutMillis = "/healthz", http.StatusOK, 2, 1000
	case hostdproto.UpdateGateDrain:
		request.PreviousVersion, request.TimeoutMillis = "2026.09.02.1", 1000
	case hostdproto.UpdateGateStability:
		request.Path, request.ExpectedStatus, request.Samples = "/healthz", http.StatusOK, 2
		request.WindowMillis, request.IntervalMillis = 1, 1
	case hostdproto.UpdateGateRollback:
		request.PreviousVersion, request.Path, request.ExpectedStatus, request.Samples, request.TimeoutMillis = "2026.09.02.1", "/healthz", http.StatusOK, 2, 1000
	}
	return request
}

func TestStandaloneUpdateGateCompletesExactLifecycleAndReload(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "gate.json")
	workloads := hostdproto.WorkloadStatus{Generation: 1}
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	gate, err := newStandaloneUpdateGate(standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: statePath, Health: health, Workloads: func() hostdproto.WorkloadStatus { return workloads }})
	if err != nil {
		t.Fatal(err)
	}
	targetResponse, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateTarget, nil))
	if err != nil || targetResponse.Target.Scope != hostdproto.UpdateGateScopeStandalone || targetResponse.Target.TunnelID != "" {
		t.Fatalf("target=%+v err=%v", targetResponse.Target, err)
	}
	target := targetResponse.Target
	for _, operation := range []string{hostdproto.UpdateGateCandidate, hostdproto.UpdateGateDrain, hostdproto.UpdateGateStability, hostdproto.UpdateGateCommit} {
		if _, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(operation, &target)); err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	}
	reloaded, err := newStandaloneUpdateGate(standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: statePath, Health: health, Workloads: func() hostdproto.WorkloadStatus { return workloads }, BeginUpdate: func(context.Context, string) error { return errors.New("committed recovery must not drain again") }, EndUpdate: func(string) error { return errors.New("committed recovery must not reopen admission") }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateCommit, &target)); err != nil {
		t.Fatalf("committed replay: %v", err)
	}
	// Final metadata commit can succeed before helper cleanup. The same signed
	// transaction must still validate health after durable gate reload.
	request := standaloneGateRequest(hostdproto.UpdateGateStability, &target)
	if _, err := reloaded.HandleUpdateGate(context.Background(), request); err != nil {
		t.Fatalf("committed stability after reload: %v", err)
	}
	for _, mutate := range []func(*hostdproto.UpdateGateRequest){
		func(r *hostdproto.UpdateGateRequest) { r.Path = "/other" },
		func(r *hostdproto.UpdateGateRequest) { r.ExpectedStatus = http.StatusNoContent },
		func(r *hostdproto.UpdateGateRequest) { r.Samples++ },
		func(r *hostdproto.UpdateGateRequest) { r.ManifestSHA256 = strings.Repeat("b", 64) },
		func(r *hostdproto.UpdateGateRequest) { r.Version = "2026.09.02.3" },
		func(r *hostdproto.UpdateGateRequest) {
			target := *r.ExpectedTarget
			target.MachineID = "another_machine"
			r.ExpectedTarget = &target
		},
	} {
		wrong := request
		mutate(&wrong)
		if _, err := reloaded.HandleUpdateGate(context.Background(), wrong); err == nil {
			t.Fatal("committed revalidation accepted a different signed policy")
		}
	}
	reloaded.config.Health = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if _, err := reloaded.HandleUpdateGate(context.Background(), request); err == nil {
		t.Fatal("committed recovery ignored failed health check")
	}

}

func TestStandaloneUpdateGateCompletesWithNoProtectedWorkloads(t *testing.T) {
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	gate, err := newStandaloneUpdateGate(standaloneUpdateGateConfig{
		MachineID: "machine_01", StatePath: filepath.Join(t.TempDir(), "gate.json"), Health: health,
		Workloads: func() hostdproto.WorkloadStatus { return hostdproto.WorkloadStatus{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	targetResponse, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateTarget, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := targetResponse.Target
	for _, operation := range []string{hostdproto.UpdateGateCandidate, hostdproto.UpdateGateDrain, hostdproto.UpdateGateStability, hostdproto.UpdateGateCommit} {
		if _, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(operation, &target)); err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	}
}

func TestStandaloneUpdateGateDefersTerminalsAndRestoresAdmissionFence(t *testing.T) {
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	busy, held := true, ""
	workloads := hostdproto.WorkloadStatus{Generation: 2, Protected: 3}
	config := standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: filepath.Join(t.TempDir(), "gate.json"), Health: health,
		Workloads: func() hostdproto.WorkloadStatus { return workloads },
		BeginUpdate: func(_ context.Context, id string) error {
			if busy {
				return execprocess.ErrUpdateInProgress
			}
			if held != "" && held != id {
				return execprocess.ErrUpdateInProgress
			}
			held = id
			return nil
		},
		EndUpdate: func(id string) error {
			if held != id {
				return execprocess.ErrUpdateInProgress
			}
			held = ""
			return nil
		}}
	gate, err := newStandaloneUpdateGate(config)
	if err != nil {
		t.Fatal(err)
	}
	response, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateTarget, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := response.Target
	if _, err = gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateCandidate, &target)); err != nil {
		t.Fatal(err)
	}
	response, err = gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateDrain, &target))
	if !errors.Is(err, execprocess.ErrUpdateInProgress) || response.BlockedReason != "" || held != "" || gate.transactions["transaction_01"].Drained {
		t.Fatalf("busy response=%+v held=%q err=%v", response, held, err)
	}
	busy = false
	if _, err = gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateDrain, &target)); err != nil {
		t.Fatal(err)
	}
	if held != "transaction_01" {
		t.Fatal("missing admission fence")
	}
	// A restarted host restores the exec fence before admitting new commands.
	held = ""
	workloads = hostdproto.WorkloadStatus{Generation: 1}
	gate, err = newStandaloneUpdateGate(config)
	if err != nil {
		t.Fatal(err)
	}
	if held != "transaction_01" {
		t.Fatal("restart lost admission fence")
	}
	for _, op := range []string{hostdproto.UpdateGateStability, hostdproto.UpdateGateCommit} {
		if _, err = gate.HandleUpdateGate(context.Background(), standaloneGateRequest(op, &target)); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	if held != "" {
		t.Fatal("commit retained admission fence")
	}
}

func TestStandaloneUpdateGateCompletesWithStableProtectedWorkloads(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "gate.json")
	workloads := hostdproto.WorkloadStatus{Generation: 7, Protected: 4}
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	config := standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: statePath, Health: health, Workloads: func() hostdproto.WorkloadStatus { return workloads }}
	gate, err := newStandaloneUpdateGate(config)
	if err != nil {
		t.Fatal(err)
	}
	targetResponse, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateTarget, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := targetResponse.Target
	for _, operation := range []string{hostdproto.UpdateGateCandidate, hostdproto.UpdateGateDrain} {
		if _, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(operation, &target)); err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	}
	reloaded, err := newStandaloneUpdateGate(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{hostdproto.UpdateGateStability, hostdproto.UpdateGateCommit} {
		if _, err := reloaded.HandleUpdateGate(context.Background(), standaloneGateRequest(operation, &target)); err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	}
}

func TestStandaloneUpdateGateRollsBackAfterInvalidWorkloadSnapshot(t *testing.T) {
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	valid := false
	gate, err := newStandaloneUpdateGate(standaloneUpdateGateConfig{
		MachineID: "machine_01",
		StatePath: filepath.Join(t.TempDir(), "gate.json"),
		Health:    health,
		Workloads: func() hostdproto.WorkloadStatus {
			if valid {
				return hostdproto.WorkloadStatus{Generation: 2, Protected: 1}
			}
			return hostdproto.WorkloadStatus{Protected: 1}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	targetResponse, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateTarget, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := targetResponse.Target
	if _, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateCandidate, &target)); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateDrain, &target)); !errors.Is(err, errStandaloneUpdateGate) {
		t.Fatalf("protected workload drain error=%v", err)
	}
	valid = true
	if _, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateRollback, &target)); err != nil {
		t.Fatalf("rollback after failed drain: %v", err)
	}
	if !gate.transactions["transaction_01"].RolledBack {
		t.Fatal("rollback did not retain exact completion receipt")
	}
	gate, err = newStandaloneUpdateGate(gate.config)
	if err != nil {
		t.Fatal(err)
	}
	// The production deployment provider resolves CurrentTarget again before
	// replaying rollback; retaining a cached target would miss this boundary.
	replayedTarget, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateTarget, nil))
	if err != nil || replayedTarget.Target != target {
		t.Fatalf("resolve completed rollback target after restart: target=%+v err=%v", replayedTarget.Target, err)
	}
	for _, field := range []string{"manifest", "version"} {
		wrongTarget := standaloneGateRequest(hostdproto.UpdateGateTarget, nil)
		if field == "manifest" {
			wrongTarget.ManifestSHA256 = strings.Repeat("b", 64)
		} else {
			wrongTarget.Version = "2026.09.08.999"
		}
		if _, err := gate.HandleUpdateGate(context.Background(), wrongTarget); !errors.Is(err, errStandaloneUpdateGate) {
			t.Fatalf("wrong %s accepted for completed target lookup: %v", field, err)
		}
	}
	if _, err = gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateRollback, &target)); err != nil {
		t.Fatalf("rollback retry after restart: %v", err)
	}
	wrong := standaloneGateRequest(hostdproto.UpdateGateRollback, &target)
	wrong.ManifestSHA256 = strings.Repeat("b", 64)
	if _, err = gate.HandleUpdateGate(context.Background(), wrong); !errors.Is(err, errStandaloneUpdateGate) {
		t.Fatalf("wrong rollback receipt accepted: %v", err)
	}
}

func TestHostsAlwaysExposeUpdateGate(t *testing.T) {
	root := t.TempDir()
	runtimeConfig := runtimeconfig.Config{StateRoot: root, Version: "test", Limits: runtimeconfig.DefaultLimits, Resources: runtimeconfig.DefaultResources}
	host, err := NewHost(context.Background(), HostConfig{Runtime: runtimeConfig, ListenAddress: "127.0.0.1:0", WorkspaceRoot: root, MachineID: "machine_01", InboxPath: func() (string, error) { return root, nil }}, HostDependencies{
		SessionLauncherFactory: machineTestLauncherFactory, Authorizer: func(string) (server.Authorizer, error) { return hostAuthorizer{}, nil }, Connector: machineServiceStub{}, RuntimeObservationService: machineServiceStub{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown(context.Background())
	if host.UpdateGate() == nil {
		t.Fatal("machine runtime omitted standalone update gate")
	}
}

// fenceFixture mimics the transfer service: one transaction at a time may hold
// the mutation fence, and another one's drain is refused while it is held.
type fenceFixture struct {
	mu   sync.Mutex
	held string
}

func (f *fenceFixture) holder() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

func (f *fenceFixture) config(statePath string, lease time.Duration) standaloneUpdateGateConfig {
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	return standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: statePath, Health: health, FenceLease: lease,
		Workloads: func() hostdproto.WorkloadStatus { return hostdproto.WorkloadStatus{Generation: 1} },
		BeginUpdate: func(_ context.Context, id string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.held != "" && f.held != id {
				return &filetransfer.Error{Code: filetransfer.ResourceLimit}
			}
			f.held = id
			return nil
		},
		EndUpdate: func(id string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.held == id {
				f.held = ""
			}
			return nil
		}}
}

func drainedGate(t *testing.T, gate *standaloneUpdateGate, id string) hostdproto.UpdateGateTargetBinding {
	t.Helper()
	with := func(request hostdproto.UpdateGateRequest) hostdproto.UpdateGateRequest {
		request.TransactionID = id
		return request
	}
	response, err := gate.HandleUpdateGate(context.Background(), with(standaloneGateRequest(hostdproto.UpdateGateTarget, nil)))
	if err != nil {
		t.Fatal(err)
	}
	target := response.Target
	for _, operation := range []string{hostdproto.UpdateGateCandidate, hostdproto.UpdateGateDrain} {
		if _, err := gate.HandleUpdateGate(context.Background(), with(standaloneGateRequest(operation, &target))); err != nil {
			t.Fatalf("%s %s: %v", id, operation, err)
		}
	}
	return target
}

// An update whose helper died after the drain (the candidate failed to start and
// the native rollback never reached the gate) left its transaction drained. Every
// later drain, and every file transfer, was then refused for good.
func TestStandaloneUpdateGateNewDrainSupersedesAbandonedTransaction(t *testing.T) {
	fence := &fenceFixture{}
	statePath := filepath.Join(t.TempDir(), "gate.json")
	gate, err := newStandaloneUpdateGate(fence.config(statePath, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stale := drainedGate(t, gate, "transaction_stale")
	// hostd restarts into the restored fence, as it does after the failed update.
	gate, err = newStandaloneUpdateGate(fence.config(statePath, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if fence.holder() != "transaction_stale" {
		t.Fatalf("restart did not restore the fence: %q", fence.holder())
	}
	drainedGate(t, gate, "transaction_next")
	if fence.holder() != "transaction_next" || !gate.transactions["transaction_stale"].RolledBack || gate.transactions["transaction_stale"].Drained {
		t.Fatalf("holder=%q stale=%+v", fence.holder(), gate.transactions["transaction_stale"])
	}
	rollback := standaloneGateRequest(hostdproto.UpdateGateRollback, &stale)
	rollback.TransactionID = "transaction_stale"
	if _, err := gate.HandleUpdateGate(context.Background(), rollback); err != nil {
		t.Fatalf("late rollback receipt for the abandoned transaction: %v", err)
	}
	if fence.holder() != "transaction_next" {
		t.Fatalf("rollback of the abandoned transaction released the new fence: %q", fence.holder())
	}
	if _, err := newStandaloneUpdateGate(fence.config(statePath, time.Hour)); err != nil {
		t.Fatalf("state with an abandoned transaction does not reload: %v", err)
	}
}

func TestStandaloneUpdateGateFenceLeaseReleasesAbandonedDrain(t *testing.T) {
	fence := &fenceFixture{}
	statePath := filepath.Join(t.TempDir(), "gate.json")
	gate, err := newStandaloneUpdateGate(fence.config(statePath, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	drainedGate(t, gate, "transaction_01")
	waitFenceReleased(t, fence)
	waitStateRecordsRollback(t, statePath)
	reloaded, err := newStandaloneUpdateGate(fence.config(statePath, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if fence.holder() != "" || !reloaded.transactions["transaction_01"].RolledBack {
		t.Fatalf("expired lease was not recorded: holder=%q %+v", fence.holder(), reloaded.transactions["transaction_01"])
	}
	// A restarted hostd gets a fresh lease for a transaction that is still pending.
	drainedGate(t, reloaded, "transaction_02")
	reloaded, err = newStandaloneUpdateGate(fence.config(statePath, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if fence.holder() != "transaction_02" {
		t.Fatal("restart lost the fence of a pending update")
	}
	waitFenceReleased(t, fence)
}

// The gate frees the fence before it persists the rollback, so a test that reloads the state right
// after the release can read the old record (it did on a slow macOS runner). Wait for the file.
func waitStateRecordsRollback(t *testing.T, statePath string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if body, err := os.ReadFile(statePath); err == nil && bytes.Contains(body, []byte(`"RolledBack":true`)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned drain was released but its rollback was never persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitFenceReleased(t *testing.T, fence *fenceFixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for fence.holder() != "" {
		if time.Now().After(deadline) {
			t.Fatal("fence lease never released an abandoned drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStandaloneUpdateGatePurgeUsesBoundedLedger(t *testing.T) {
	now := time.Now().UTC()
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	gate, err := newStandaloneUpdateGate(standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: filepath.Join(t.TempDir(), "gate.json"), Health: health, Workloads: func() hostdproto.WorkloadStatus { return hostdproto.WorkloadStatus{Generation: 1} }, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateTarget, nil)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Hour)
	gate.purge()
	if len(gate.transactions) != 0 {
		t.Fatal("expired standalone transaction retained")
	}
}

func TestWorkerUpdateDrainWaitsForCommittedTransferChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	durable, err := store.Open(ctx, store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	transfers, err := filetransfer.New(filetransfer.Config{Root: filepath.Join(root, "transfers"), LocalMachineID: "machine_local", Store: durable})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("chunk")
	sum := sha256.Sum256(data)
	created, err := transfers.Create(ctx, filetransfer.CreateRequest{BatchID: "fb_update", SourceMachineID: "machine_source", DestinationMachineID: "machine_local", InitiatingUserID: "user_1", Files: []filetransfer.File{{Basename: "chunk.bin", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}}})
	if err != nil {
		t.Fatal(err)
	}
	host := &Host{transfers: transfers}
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	gate, err := newStandaloneUpdateGate(standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: filepath.Join(root, "gate.json"), Health: health, Workloads: host.WorkloadStatus, BeginUpdate: host.beginTransferUpdate, EndUpdate: host.endTransferUpdate})
	if err != nil {
		t.Fatal(err)
	}
	response, err := gate.HandleUpdateGate(ctx, standaloneGateRequest(hostdproto.UpdateGateTarget, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := response.Target
	if _, err := gate.HandleUpdateGate(ctx, standaloneGateRequest(hostdproto.UpdateGateCandidate, &target)); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	appendDone := make(chan error, 1)
	go func() { _, err := transfers.Append(ctx, created[0].ID, 0, reader); appendDone <- err }()
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	// A failed attempt must reopen transfer admission without interrupting the
	// admitted writer or resetting its resume offset.
	interrupted, stopInterrupted := context.WithTimeout(ctx, 20*time.Millisecond)
	_, drainErr := gate.HandleUpdateGate(interrupted, standaloneGateRequest(hostdproto.UpdateGateDrain, &target))
	stopInterrupted()
	if !errors.Is(drainErr, context.DeadlineExceeded) {
		t.Fatalf("interrupted drain=%v", drainErr)
	}
	if transfers.ActiveCount() != 1 {
		t.Fatal("interrupted drain stopped admitted writer")
	}
	reopened := filetransfer.CreateRequest{BatchID: "fb_reopened", SourceMachineID: "machine_source", DestinationMachineID: "machine_local", InitiatingUserID: "user_1", Files: []filetransfer.File{{Basename: "another.bin", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}}}
	if _, err := transfers.Create(ctx, reopened); err != nil {
		t.Fatalf("interrupted drain stranded admission: %v", err)
	}
	drainDone := make(chan error, 1)
	go func() {
		_, err := gate.HandleUpdateGate(ctx, standaloneGateRequest(hostdproto.UpdateGateDrain, &target))
		drainDone <- err
	}()
	select {
	case err := <-drainDone:
		t.Fatalf("worker drain finished before chunk commit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-appendDone; err != nil {
		t.Fatal(err)
	}
	if err := <-drainDone; err != nil {
		t.Fatal(err)
	}
	stored, err := transfers.Get(ctx, created[0].ID)
	if err != nil || stored.CommittedOffset != int64(len(data)) {
		t.Fatalf("committed offset=%d err=%v", stored.CommittedOffset, err)
	}
	if _, err := transfers.Complete(ctx, created[0].ID); !transferUpdateBusy(err) {
		t.Fatalf("drained worker admitted publication: %v", err)
	}
	if err := transfers.EndUpdate("different_transaction"); !transferUpdateBusy(err) {
		t.Fatalf("different transaction released fence: %v", err)
	}
	if _, err := transfers.Append(ctx, created[0].ID, int64(len(data)), strings.NewReader("")); !transferUpdateBusy(err) {
		t.Fatalf("drained worker admitted chunk: %v", err)
	}
	if _, err := gate.HandleUpdateGate(ctx, standaloneGateRequest(hostdproto.UpdateGateRollback, &target)); err != nil {
		t.Fatal(err)
	}
	if _, err := transfers.Complete(ctx, created[0].ID); err != nil {
		t.Fatalf("rollback did not reopen transfer publication: %v", err)
	}
	if _, err := gate.HandleUpdateGate(ctx, standaloneGateRequest(hostdproto.UpdateGateStability, &target)); err == nil {
		t.Fatal("rolled-back transaction authorized stability revalidation")
	}
}

func transferUpdateBusy(err error) bool {
	var failure *filetransfer.Error
	return errors.As(err, &failure) && failure.Code == filetransfer.ResourceLimit
}
