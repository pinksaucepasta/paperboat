//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

type optionalSSHAuthority struct {
	starts, stops atomic.Int32
	failCleanup   atomic.Bool
}

func (a *optionalSSHAuthority) Start(context.Context) error { a.starts.Add(1); return nil }
func (a *optionalSSHAuthority) Shutdown(context.Context) error {
	a.stops.Add(1)
	if a.failCleanup.Load() {
		return errors.New("cleanup failed")
	}
	return nil
}

func waitSSHHealth(t *testing.T, s *managedSSHService, state health.State, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if got := s.CapabilityHealth(); got.State == state && got.Reason == reason {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("SSH health=%+v want=%s/%s", s.CapabilityHealth(), state, reason)
		case <-tick.C:
		}
	}
}
func applySSHSelection(t *testing.T, gate *machineCapabilityController, version uint64, enabled bool) {
	t.Helper()
	if err := gate.Apply(t.Context(), machineCapabilityPolicy{Schema: machineCapabilitiesSchemaV1, DesiredVersion: version, Desired: machineCapabilitySelection{ManagedSSH: enabled}}); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalSSHDoesNotInitializeDisabledCapability(t *testing.T) {
	gate := newMachineCapabilityController(nil)
	var attempts atomic.Int32
	service := &managedSSHService{gate: gate, interval: time.Millisecond, timeout: time.Second, initialize: func(context.Context) (Service, error) { attempts.Add(1); return &optionalSSHAuthority{}, nil }}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.Disable(t.Context()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 0 {
		t.Fatal("disabled SSH initialized local target or requested authority")
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalSSHFailureKeepsUnifiedRuntimeUsableAndRecovers(t *testing.T) {
	gate := newMachineCapabilityController(nil)
	applySSHSelection(t, gate, 1, true)
	authority := &optionalSSHAuthority{}
	var allowRecovery atomic.Bool
	service := &managedSSHService{gate: gate, interval: time.Millisecond, timeout: time.Second, initialize: func(context.Context) (Service, error) {
		if !allowRecovery.Load() {
			return nil, ErrManagedSSHUnavailable
		}
		return authority, nil
	}}
	root := t.TempDir()
	host, err := NewHost(t.Context(), HostConfig{Runtime: runtimeconfig.Config{StateRoot: root, Version: "test", Limits: runtimeconfig.DefaultLimits, Resources: runtimeconfig.DefaultResources}, ListenAddress: "127.0.0.1:0", WorkspaceRoot: root, MachineID: "machine_test", InboxPath: root}, HostDependencies{Authorizer: func(string) (server.Authorizer, error) { return hostAuthorizer{}, nil }, SessionLauncherFactory: machineTestLauncherFactory, ManagedSSHService: service, Capabilities: gate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Shutdown(context.Background()) })
	gate.SetReconciler(host.reconcileMachineCapabilities)
	if err := host.StartStable(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitSSHHealth(t, service, health.Unavailable, "target_or_authority_unavailable")
	if host.State() != Running || host.sessions == nil || host.executions == nil || host.transfers == nil {
		t.Fatal("SSH target failure lost independent machine services")
	}
	if got := host.health.Snapshot().Capabilities["ssh.v1"]; got.State != health.Unavailable {
		t.Fatalf("SSH readiness was fabricated: %+v", got)
	}
	allowRecovery.Store(true)
	waitSSHHealth(t, service, health.Ready, "")
	authority.failCleanup.Store(true)
	if err := gate.Apply(t.Context(), machineCapabilityPolicy{Schema: machineCapabilitiesSchemaV1, DesiredVersion: 2}); err == nil {
		t.Fatal("SSH disable acknowledged failed key cleanup")
	}
	authority.failCleanup.Store(false)
	if gate.Enabled("ssh.v1") {
		t.Fatal("cleanup failure reopened admission")
	}
	waitSSHHealth(t, service, health.Unavailable, "disabled")
	applySSHSelection(t, gate, 3, true)
	waitSSHHealth(t, service, health.Ready, "")
	if authority.starts.Load() != 2 {
		t.Fatalf("authority starts=%d want2", authority.starts.Load())
	}
	if err := host.ShutdownStable(t.Context()); err != nil {
		t.Fatal(err)
	}
	if authority.stops.Load() < 3 {
		t.Fatal("SSH authority cleanup was omitted")
	}
}

func TestOptionalSSHDisableCancelsPendingInitialization(t *testing.T) {
	gate := newMachineCapabilityController(nil)
	applySSHSelection(t, gate, 1, true)
	entered := make(chan struct{})
	authority := &optionalSSHAuthority{}
	service := &managedSSHService{gate: gate, interval: time.Millisecond, timeout: time.Second, initialize: func(ctx context.Context) (Service, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("SSH initialization did not start")
	}
	applySSHSelection(t, gate, 2, false)
	if err := service.Disable(t.Context()); err != nil {
		t.Fatal(err)
	}
	if authority.starts.Load() != 0 {
		t.Fatal("disabled pending SSH requested authority")
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalSSHFirstDisabledPolicyCleansCrashLeftGrants(t *testing.T) {
	gate := newMachineCapabilityController(nil)
	var cleans, authorizations atomic.Int32
	var cleanupFailed atomic.Bool
	cleanupFailed.Store(true)
	service := &managedSSHService{gate: gate, interval: time.Millisecond, timeout: time.Second, cleanup: func(context.Context) error {
		cleans.Add(1)
		if cleanupFailed.Load() {
			return errors.New("persisted keys inaccessible")
		}
		return nil
	}, initialize: func(context.Context) (Service, error) { authorizations.Add(1); return &optionalSSHAuthority{}, nil }}
	// Policy can arrive before the optional stable component starts. Its cleanup
	// acknowledgement still must cover persisted grants from a crashed process.
	if err := service.Disable(t.Context()); err == nil {
		t.Fatal("first disabled policy acknowledged inaccessible persisted grants")
	}
	cleanupFailed.Store(false)
	if err := service.Disable(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.Disable(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cleans.Load() < 3 || authorizations.Load() != 0 {
		t.Fatalf("cleanup=%d authority=%d", cleans.Load(), authorizations.Load())
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}
