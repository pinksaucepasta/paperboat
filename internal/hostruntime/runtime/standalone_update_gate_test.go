//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
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
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil)
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
	reloaded, err := newStandaloneUpdateGate(standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: statePath, Health: health, Workloads: func() hostdproto.WorkloadStatus { return workloads }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateCommit, &target)); err != nil {
		t.Fatalf("committed replay: %v", err)
	}
}

func TestStandaloneUpdateGateCompletesWithNoProtectedWorkloads(t *testing.T) {
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil)
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
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil)
	busy, held := true, ""
	workloads := hostdproto.WorkloadStatus{Generation: 2, Protected: 3}
	config := standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: filepath.Join(t.TempDir(), "gate.json"), Health: health,
		Workloads: func() hostdproto.WorkloadStatus { return workloads },
		BeginUpdate: func(id string) error {
			if busy {
				return session.ErrUpdateBusy
			}
			if held != "" && held != id {
				return session.ErrUpdateInProgress
			}
			held = id
			return nil
		},
		EndUpdate: func(id string) error {
			if held != id {
				return session.ErrUpdateInProgress
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
	if err != nil || response.BlockedReason != hostdproto.UpdateGateBlockedActiveTerminalSessions || held != "" || gate.transactions["transaction_01"].Drained {
		t.Fatalf("busy response=%+v held=%q err=%v", response, held, err)
	}
	busy = false
	if _, err = gate.HandleUpdateGate(context.Background(), standaloneGateRequest(hostdproto.UpdateGateDrain, &target)); err != nil {
		t.Fatal(err)
	}
	if held != "transaction_01" {
		t.Fatal("missing admission fence")
	}
	// A restarted host restores the fence before admitting new shells. Transfer
	// counts may change across restart; they cannot stand in for a terminal fence.
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
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil)
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
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil)
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
	runtimeConfig := runtimeconfig.Config{Profile: runtimeconfig.BYOD, StateRoot: root, Version: "test", Limits: runtimeconfig.DefaultLimits, Resources: runtimeconfig.DefaultResources}
	host, err := NewClientCoordinator(context.Background(), HostConfig{Runtime: runtimeConfig, ListenAddress: "127.0.0.1:0", WorkspaceRoot: root, MachineID: "machine_01", InboxPath: root}, HostDependencies{
		Authorizer: func(string) (server.Authorizer, error) { return hostAuthorizer{}, nil }, Connector: clientServiceStub{}, RuntimeObservationService: clientServiceStub{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown(context.Background())
	if host.UpdateGate() == nil {
		t.Fatal("client coordinator omitted standalone update gate")
	}
}

func TestStandaloneUpdateGatePurgeUsesBoundedLedger(t *testing.T) {
	now := time.Now().UTC()
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil)
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
