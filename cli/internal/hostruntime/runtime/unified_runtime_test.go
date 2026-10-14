package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type machineServiceStub struct{}

func (machineServiceStub) Start(context.Context) error    { return nil }
func (machineServiceStub) Shutdown(context.Context) error { return nil }
func (machineServiceStub) Capabilities() []string         { return nil }

type machineLifecycleService struct {
	starts    int
	shutdowns int
}

func (s *machineLifecycleService) Start(context.Context) error { s.starts++; return nil }
func (s *machineLifecycleService) Shutdown(context.Context) error {
	s.shutdowns++
	return nil
}

func TestUnifiedRuntimeOwnsTerminalResources(t *testing.T) {
	root := t.TempDir()
	host, err := NewHost(context.Background(), HostConfig{Runtime: runtimeconfig.Config{StateRoot: root, Version: "test", Limits: runtimeconfig.DefaultLimits, Resources: runtimeconfig.DefaultResources}, ListenAddress: "127.0.0.1:0", WorkspaceRoot: root, InboxPath: filepath.Join(root, "Inbox"), MachineID: "machine_test"}, HostDependencies{Authorizer: func(string) (server.Authorizer, error) { return hostAuthorizer{}, nil }, SessionLauncherFactory: machineTestLauncherFactory, Capabilities: newMachineCapabilityController(nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Shutdown(context.Background()) })
	if host.sessions == nil || host.executions == nil || host.dispatcher == nil {
		t.Fatal("unified runtime omitted terminal ownership")
	}
	for _, capability := range []string{"terminal.v1", "exec.v1", "ssh.v1"} {
		outcome := host.dispatcher.Handle(context.Background(), server.Authorization{}, capability, json.RawMessage(`{}`))
		if outcome.ErrorCode != "capability_disabled" {
			t.Fatalf("disabled %s admitted: %+v", capability, outcome)
		}
	}
	if len(host.sessions.List()) != 0 || len(host.executions.ActiveSnapshots()) != 0 {
		t.Fatal("disabled capabilities created workloads")
	}
	response := httptest.NewRecorder()
	host.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/runtime", nil))
	if response.Code == http.StatusNotFound {
		t.Fatal("unified runtime omitted terminal route")
	}
}

type machineTestLauncher struct{}

func (machineTestLauncher) Launch(context.Context, process.LaunchRequest) (session.Snapshot, error) {
	return session.Snapshot{}, errors.New("session launch is outside lifecycle check")
}
func machineTestLauncherFactory(session.Service) (server.SessionLauncher, error) {
	return machineTestLauncher{}, nil
}

func TestUnifiedRuntimeSupportsStableLifecycle(t *testing.T) {
	root := t.TempDir()
	peer := &machineLifecycleService{}
	host, err := NewHost(context.Background(), HostConfig{
		Runtime:       runtimeconfig.Config{StateRoot: root, Version: "test", Limits: runtimeconfig.DefaultLimits, Resources: runtimeconfig.DefaultResources},
		ListenAddress: "127.0.0.1:0", WorkspaceRoot: root, InboxPath: filepath.Join(root, "Inbox"), MachineID: "machine_test",
	}, HostDependencies{
		SessionLauncherFactory: machineTestLauncherFactory,
		Authorizer:             func(string) (server.Authorizer, error) { return hostAuthorizer{}, nil },
		Connector:              machineServiceStub{}, RuntimeObservationService: machineServiceStub{},
		NativePeerFactory: func(func(net.Conn) error, http.Handler) (Service, error) { return peer, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.StartStable(t.Context()); err != nil {
		t.Fatalf("start stable machine runtime: %v", err)
	}
	if peer.starts != 1 {
		t.Fatalf("machine peer poller starts = %d, want 1", peer.starts)
	}
	if host.State() != Running {
		t.Fatalf("machine coordination state = %q, want %q", host.State(), Running)
	}
	health := host.health.Snapshot()
	if capability := health.Capabilities["worker_lifecycle"]; capability.State != "ready" {
		t.Fatalf("machine worker lifecycle health = %#v, want ready", capability)
	}
	status := host.WorkloadStatus()
	if status.Generation == 0 {
		t.Fatalf("workload status = %#v, want initialized generation", status)
	}
	if err := host.ShutdownStable(context.Background()); err != nil {
		t.Fatalf("shutdown stable machine runtime: %v", err)
	}
	if peer.shutdowns != 1 {
		t.Fatalf("machine peer poller shutdowns = %d, want 1", peer.shutdowns)
	}
}
