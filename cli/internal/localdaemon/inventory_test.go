package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type machineResult struct {
	machines []api.UserMachine
	err      error
}

type scriptedMachineSource struct {
	mu      sync.Mutex
	results []machineResult
	index   int
}

func (*scriptedMachineSource) APIHost() string { return "api.example.test" }

// apiDown is the Paperboat API not answering a request.
func apiDown(path string) error {
	return &url.Error{Op: "Get", URL: "https://api.example.test" + path, Err: context.DeadlineExceeded}
}

type completionMachineSource struct{ *scriptedMachineSource }

func (s completionMachineSource) ListCompletionItems(context.Context, []api.UserMachine) ([]localapi.CompletionItem, error) {
	return []localapi.CompletionItem{{Kind: "machine", Value: "studio", Description: "Studio - ready", EnvironmentID: "environment_1"}}, nil
}

func (s *scriptedMachineSource) ListUserMachines(context.Context) ([]api.UserMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.index
	if index >= len(s.results) {
		index = len(s.results) - 1
	} else {
		s.index++
	}
	return s.results[index].machines, s.results[index].err
}

func newTestInventory(t *testing.T, source MachineSource, clock func() time.Time) (*Inventory, *localapi.SnapshotStore) {
	t.Helper()
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Second, RequestTimeout: time.Second, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return inventory, store
}

func TestInventoryPublishesSortedAuthoritativeMachines(t *testing.T) {
	now := time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)
	observed := now.Add(-time.Minute)
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{
		{ID: "machine_2", Alias: "studio-mac", InstallationGeneration: 0, Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true}}},
		{ID: "machine_1", Alias: "build-host", Online: true, InstallationGeneration: 7, SSHAuthority: api.SSHAuthority{TargetGeneration: 7, HostKeyGeneration: 7}, SSHLocalReady: true, RuntimeDiagnostics: api.RuntimeDiagnostics{ObservedAt: &observed}, Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true, Observed: true}, PreviewLaunch: api.MachineCapability{Configured: true, Observed: true}}},
		{ID: "machine_3", Alias: "build-host", State: "revoked", InstallationGeneration: 2},
	}}}}
	inventory, store := newTestInventory(t, source, func() time.Time { return now })
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != 1 || snapshot.DaemonState != "ready" || !snapshot.ObservedAt.Equal(now) || len(snapshot.Machines) != 3 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	ready, revoked, stopped := snapshot.Machines[0], snapshot.Machines[1], snapshot.Machines[2]
	if ready.ID != "machine_1" || ready.RuntimeState != "ready" || !ready.Eligible || ready.Generation != 7 || ready.TransferReadiness != "ready" || ready.PreviewReadiness != "ready" || ready.SSHReadiness != "ready" || ready.LastObservedAt == nil || !ready.LastObservedAt.Equal(observed) {
		t.Fatalf("ready=%#v", ready)
	}
	if revoked.ID != "machine_3" || revoked.RuntimeState != "failed" || revoked.Eligible || len(revoked.Health) != 1 || revoked.Health[0].Code != "machine_unavailable" {
		t.Fatalf("revoked=%#v", revoked)
	}
	if stopped.ID != "machine_2" || stopped.RuntimeState != "stopped" || stopped.Eligible || stopped.Generation != 0 || stopped.TransferReadiness != "degraded" || stopped.PreviewReadiness != "unavailable" {
		t.Fatalf("stopped=%#v", stopped)
	}
}

func TestInventoryRefreshRetainsDaemonVersionAcrossPublications(t *testing.T) {
	now := time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)
	controlPlaneErr := errors.New("control plane unavailable")
	source := &scriptedMachineSource{results: []machineResult{
		{machines: []api.UserMachine{{ID: "machine_1", Alias: "before", InstallationGeneration: 1}}},
		{err: controlPlaneErr},
		{machines: []api.UserMachine{{ID: "machine_1", Alias: "after", InstallationGeneration: 1}}},
	}}
	inventory, store := newTestInventory(t, source, func() time.Time { return now })

	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.DaemonVersion != buildinfo.Version {
		t.Fatalf("first daemon version=%q want=%q", first.DaemonVersion, buildinfo.Version)
	}

	if err := inventory.Refresh(context.Background()); !errors.Is(err, controlPlaneErr) {
		t.Fatalf("degraded refresh err=%v", err)
	}
	degraded, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if degraded.Generation != first.Generation+1 || degraded.DaemonVersion != buildinfo.Version {
		t.Fatalf("degraded snapshot=%#v want generation=%d version=%q", degraded, first.Generation+1, buildinfo.Version)
	}

	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Generation != degraded.Generation+1 || recovered.DaemonVersion != buildinfo.Version {
		t.Fatalf("recovered snapshot=%#v want generation=%d version=%q", recovered, degraded.Generation+1, buildinfo.Version)
	}
}

func TestInventoryDoesNotPublishReadyBeforeMachineReconciliation(t *testing.T) {
	now := time.Date(2026, 8, 4, 8, 30, 0, 0, time.UTC)
	store, err := localapi.NewSnapshotStore(&localapi.Snapshot{
		Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now,
		DaemonState: "starting", DaemonVersion: buildinfo.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	reconcileStarted := make(chan struct{})
	releaseReconcile := make(chan struct{})
	onMachinesState := make(chan string, 1)
	inventory, err := NewInventory(InventoryConfig{
		Source:          &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}},
		Store:           store,
		RefreshInterval: time.Second,
		RequestTimeout:  time.Second,
		Clock:           func() time.Time { return now.Add(time.Second) },
		ReconcileMachines: func(ctx context.Context, _ []api.UserMachine) error {
			close(reconcileStarted)
			select {
			case <-releaseReconcile:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		OnMachines: func(ctx context.Context, _ []api.UserMachine) {
			snapshot, snapshotErr := store.Snapshot(ctx)
			if snapshotErr != nil {
				onMachinesState <- "snapshot-error"
				return
			}
			onMachinesState <- snapshot.DaemonState
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- inventory.Refresh(context.Background()) }()
	select {
	case <-reconcileStarted:
	case <-time.After(time.Second):
		t.Fatal("machine reconciliation did not start")
	}
	beforeRelease, err := store.Snapshot(context.Background())
	if err != nil || beforeRelease.DaemonState != "starting" || beforeRelease.Generation != 1 || len(beforeRelease.Machines) != 0 {
		t.Fatalf("snapshot before reconciliation completed=%#v err=%v", beforeRelease, err)
	}
	select {
	case state := <-onMachinesState:
		t.Fatalf("post-publication callback ran before reconciliation completed with state %q", state)
	default:
	}
	close(releaseReconcile)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("inventory refresh did not finish after reconciliation was released")
	}
	afterRelease, err := store.Snapshot(context.Background())
	if err != nil || afterRelease.DaemonState != "ready" || afterRelease.Generation != 2 || len(afterRelease.Machines) != 1 {
		t.Fatalf("snapshot after reconciliation=%#v err=%v", afterRelease, err)
	}
	select {
	case state := <-onMachinesState:
		if state != "ready" {
			t.Fatalf("post-publication callback observed state %q, want ready", state)
		}
	case <-time.After(time.Second):
		t.Fatal("post-publication callback did not run")
	}
}

func TestInventoryReconciliationFailurePublishesDegradedMachinesAndRecovers(t *testing.T) {
	now := time.Date(2026, 8, 4, 8, 45, 0, 0, time.UTC)
	clockCalls := 0
	clock := func() time.Time {
		clockCalls++
		return now.Add(time.Duration(clockCalls) * time.Second)
	}
	reconcileErr := errors.New("local machine access unavailable")
	reconcileCalls := 0
	refreshErrors := make(chan error, 2)
	postPublish := make(chan localapi.Snapshot, 2)
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := NewInventory(InventoryConfig{
		Source:          &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}},
		Store:           store,
		RefreshInterval: time.Second,
		RequestTimeout:  time.Second,
		Clock:           clock,
		ReconcileMachines: func(context.Context, []api.UserMachine) error {
			reconcileCalls++
			if reconcileCalls == 1 {
				return reconcileErr
			}
			return nil
		},
		OnRefresh: func(err error) { refreshErrors <- err },
		OnMachines: func(ctx context.Context, _ []api.UserMachine) {
			snapshot, snapshotErr := store.Snapshot(ctx)
			if snapshotErr != nil {
				postPublish <- localapi.Snapshot{}
				return
			}
			postPublish <- snapshot
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inventory.setupSince = time.Unix(1, 0) // past start-up: a failure is degraded

	if err := inventory.Refresh(context.Background()); !errors.Is(err, reconcileErr) {
		t.Fatalf("degraded refresh err=%v", err)
	}
	first, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.DaemonState != "degraded" || len(first.Machines) != 1 || first.Machines[0].ID != "machine_1" || len(first.Health) != 1 {
		t.Fatalf("degraded snapshot=%#v", first)
	}
	health := first.Health[0]
	if health.Code != "local_access_unavailable" || health.Severity != "error" || health.Title != "Local machine access is unavailable" || health.Recovery != "Check Paperboat local access settings and repair the installation" || health.ETag != health.Code || health.BrokenSince == nil {
		t.Fatalf("local access health=%#v", health)
	}
	if diagnosticErr := <-refreshErrors; !errors.Is(diagnosticErr, reconcileErr) {
		t.Fatalf("refresh diagnostic error=%v", diagnosticErr)
	}
	if published := <-postPublish; published.DaemonState != "degraded" || len(published.Machines) != 1 || len(published.Health) != 1 || published.Health[0].Code != "local_access_unavailable" {
		t.Fatalf("post-publication callback observed %#v", published)
	}

	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatalf("recovered refresh err=%v", err)
	}
	second, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.DaemonState != "ready" || len(second.Machines) != 1 || len(second.Health) != 0 || second.Generation != first.Generation+1 {
		t.Fatalf("recovered snapshot=%#v", second)
	}
	if diagnosticErr := <-refreshErrors; diagnosticErr != nil {
		t.Fatalf("recovered refresh diagnostic error=%v", diagnosticErr)
	}
	if published := <-postPublish; published.DaemonState != "ready" || len(published.Health) != 0 {
		t.Fatalf("post-publication callback observed %#v", published)
	}
}

func TestInventoryBoundsMachineReconciliationByRequestTimeout(t *testing.T) {
	now := time.Date(2026, 8, 4, 9, 15, 0, 0, time.UTC)
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := NewInventory(InventoryConfig{
		Source:          &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}},
		Store:           store,
		RefreshInterval: time.Second,
		RequestTimeout:  20 * time.Millisecond,
		Clock:           func() time.Time { return now },
		ReconcileMachines: func(ctx context.Context, _ []api.UserMachine) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inventory.setupSince = time.Unix(1, 0) // past start-up: a failure is degraded
	started := time.Now()
	err = inventory.Refresh(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("refresh err=%v elapsed=%s", err, time.Since(started))
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DaemonState != "degraded" || len(snapshot.Machines) != 1 || len(snapshot.Health) != 1 || snapshot.Health[0].Code != "local_access_unavailable" {
		t.Fatalf("timed-out reconciliation snapshot=%#v", snapshot)
	}
}

func TestInventoryPublishesBoundedCompletionProjection(t *testing.T) {
	now := time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)
	source := completionMachineSource{&scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio", EnvironmentID: "environment_1"}}}}}}
	inventory, _ := newTestInventory(t, source, func() time.Time { return now })
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	projection, err := inventory.Completions(context.Background())
	if err != nil || !projection.ObservedAt.Equal(now) || len(projection.Items) != 1 || projection.Items[0].Value != "studio" {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
	projection.Items[0].Value = "changed"
	again, _ := inventory.Completions(context.Background())
	if again.Items[0].Value != "studio" {
		t.Fatal("completion projection was not cloned")
	}
}

func TestInventoryClassifiesSSHAuthorityTransitions(t *testing.T) {
	base := api.UserMachine{ID: "machine_1", Alias: "studio", Online: true, InstallationGeneration: 4, SSHLocalReady: true}
	tests := []struct {
		name      string
		machine   api.UserMachine
		readiness string
		health    string
	}{
		{name: "missing target", machine: base, readiness: "unavailable", health: "ssh_target_not_ready"},
		{name: "no ssh server (no host keys ever)", machine: func() api.UserMachine { value := base; value.SSHAuthority.TargetGeneration = 4; return value }(), readiness: "degraded", health: "ssh_host_keys_unpublished"},
		{name: "changed host keys", machine: func() api.UserMachine {
			value := base
			value.SSHAuthority = api.SSHAuthority{TargetGeneration: 4, HostKeyGeneration: 3}
			return value
		}(), readiness: "degraded", health: "ssh_host_key_changed"},
		{name: "stale generation", machine: func() api.UserMachine {
			value := base
			value.SSHAuthority = api.SSHAuthority{TargetGeneration: 3, HostKeyGeneration: 3}
			return value
		}(), readiness: "degraded", health: "ssh_target_not_ready"},
		{name: "ready", machine: func() api.UserMachine {
			value := base
			value.SSHAuthority = api.SSHAuthority{TargetGeneration: 4, HostKeyGeneration: 4}
			return value
		}(), readiness: "ready"},
		{name: "local integration starting", machine: func() api.UserMachine {
			value := base
			value.SSHAuthority = api.SSHAuthority{TargetGeneration: 4, HostKeyGeneration: 4}
			value.SSHLocalReady = false
			return value
		}(), readiness: "degraded", health: "machine_services_starting"},
		{name: "local integration unavailable", machine: func() api.UserMachine {
			value := base
			value.SSHAuthority = api.SSHAuthority{TargetGeneration: 4, HostKeyGeneration: 4}
			value.SSHLocalReady = false
			value.SSHLocalCode = "ssh_target_not_ready"
			return value
		}(), readiness: "degraded", health: "ssh_target_not_ready"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := mapMachines([]api.UserMachine{test.machine})[0]
			if status.SSHReadiness != test.readiness {
				t.Fatalf("readiness=%q status=%#v", status.SSHReadiness, status)
			}
			if test.health == "" {
				if len(status.Health) != 0 {
					t.Fatalf("health=%#v", status.Health)
				}
			} else if len(status.Health) != 1 || status.Health[0].Code != test.health {
				t.Fatalf("health=%#v", status.Health)
			}
			if test.name == "local integration unavailable" && status.Health[0].Recovery != managedSSHDoctorRecovery {
				t.Fatalf("recovery=%q", status.Health[0].Recovery)
			}
		})
	}
}

func TestInventoryProjectsOnlyTypedUpdateHealth(t *testing.T) {
	observed := time.Now().UTC()
	base := api.UserMachine{ID: "machine_1", Alias: "studio", Availability: api.AvailabilityPolicy{Schema: "paperboat.availability-policy/v1", ObservedAt: &observed}}
	for name, test := range map[string]struct {
		value string
		want  string
	}{
		"healthy":           {value: "healthy", want: "healthy"},
		"recovery required": {value: "recovery_required", want: "recovery_required"},
		"unrecognized":      {value: "journal:/private/path", want: "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			machine := base
			machine.Availability.UpdateHealth = test.value
			if got := mapMachines([]api.UserMachine{machine})[0].UpdateHealth; got != test.want {
				t.Fatalf("update health=%q want=%q", got, test.want)
			}
		})
	}
}

func TestInventoryPublishesOnlySemanticTransitionsAndPreservesLastGoodMachines(t *testing.T) {
	now := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	clockCalls := 0
	clock := func() time.Time {
		clockCalls++
		return now.Add(time.Duration(clockCalls) * time.Minute)
	}
	machines := []api.UserMachine{{ID: "machine_1", Alias: "studio", Online: true, InstallationGeneration: 3}}
	unavailable := apiDown("/v1/machines")
	source := &scriptedMachineSource{results: []machineResult{{machines: machines}, {machines: machines}, {err: unavailable}, {err: unavailable}, {machines: machines}, {machines: machines}}}
	inventory, store := newTestInventory(t, source, clock)

	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	initial, _ := store.Snapshot(context.Background())
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	duplicate, _ := store.Snapshot(context.Background())
	if duplicate.Generation != initial.Generation || !duplicate.ObservedAt.Equal(initial.ObservedAt) {
		t.Fatalf("duplicate=%#v initial=%#v", duplicate, initial)
	}
	if err := inventory.Refresh(context.Background()); !errors.Is(err, unavailable) {
		t.Fatalf("failure err=%v", err)
	}
	degraded, _ := store.Snapshot(context.Background())
	if degraded.Generation != initial.Generation+1 || degraded.DaemonState != "degraded" || len(degraded.Health) != 1 || len(degraded.Machines) != 1 || degraded.Machines[0].ID != "machine_1" {
		t.Fatalf("degraded=%#v", degraded)
	}
	if err := inventory.Refresh(context.Background()); !errors.Is(err, unavailable) {
		t.Fatalf("repeat failure err=%v", err)
	}
	repeated, _ := store.Snapshot(context.Background())
	if repeated.Generation != degraded.Generation || !repeated.ObservedAt.Equal(degraded.ObservedAt) || repeated.Health[0].BrokenSince == nil || degraded.Health[0].BrokenSince == nil || !repeated.Health[0].BrokenSince.Equal(*degraded.Health[0].BrokenSince) {
		t.Fatalf("repeated=%#v degraded=%#v", repeated, degraded)
	}
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, _ := store.Snapshot(context.Background())
	if recovered.Generation != degraded.Generation+1 || recovered.DaemonState != "ready" || len(recovered.Health) != 0 {
		t.Fatalf("recovered=%#v", recovered)
	}
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	stable, _ := store.Snapshot(context.Background())
	if stable.Generation != recovered.Generation || !stable.ObservedAt.Equal(recovered.ObservedAt) {
		t.Fatalf("stable=%#v recovered=%#v", stable, recovered)
	}
}

type blockingMachineSource struct{}

func (blockingMachineSource) ListUserMachines(ctx context.Context) ([]api.UserMachine, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestInventoryBoundsRequestsAndRunStopsOnCancellation(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	store, _ := localapi.NewSnapshotStore(nil)
	inventory, err := NewInventory(InventoryConfig{Source: blockingMachineSource{}, Store: store, RefreshInterval: time.Second, RequestTimeout: 10 * time.Millisecond, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := inventory.Refresh(context.Background()); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("err=%v elapsed=%s", err, time.Since(started))
	}
	snapshot, snapshotErr := store.Snapshot(context.Background())
	if snapshotErr != nil || snapshot.DaemonState != "degraded" || len(snapshot.Health) != 1 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, snapshotErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("inventory did not stop")
	}
}

func TestInventoryRejectsInvalidConfiguration(t *testing.T) {
	store, _ := localapi.NewSnapshotStore(nil)
	if _, err := NewInventory(InventoryConfig{Store: store}); !errors.Is(err, ErrInvalidInventoryConfig) {
		t.Fatalf("missing source err=%v", err)
	}
	if _, err := NewInventory(InventoryConfig{Source: &scriptedMachineSource{results: []machineResult{{}}}, Store: store, RefreshInterval: time.Millisecond}); !errors.Is(err, ErrInvalidInventoryConfig) {
		t.Fatalf("short interval err=%v", err)
	}
}

// An inventory change published by the server refreshes the machine list at once, although the
// periodic refresh is far away.
func TestInventoryRefreshesWhenTheServerReportsAChange(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}
	var refreshes atomic.Int32
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicInventory)
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Hour, RequestTimeout: time.Second, Events: subscriptions[0], EventRefreshSpacing: time.Millisecond, OnRefresh: func(error) { refreshes.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	waitFor := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for refreshes.Load() < want {
			if time.Now().After(deadline) {
				t.Fatalf("refreshes=%d, want %d", refreshes.Load(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(1)
	plane.Publish(machineevents.TopicInventory)
	waitFor(2)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v", err)
	}
}

// A burst of inventory changes (one machine change bumps every daemon's topic on a busy account) folds into
// one refresh per spacing instead of one machine-list download per change.
func TestInventoryFoldsChangesWithinTheSpacingIntoOneRefresh(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}
	var refreshes atomic.Int32
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicInventory)
	const spacing = 300 * time.Millisecond
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Hour, RequestTimeout: time.Second, Events: subscriptions[0], EventRefreshSpacing: spacing, OnRefresh: func(error) { refreshes.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for refreshes.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no first refresh")
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	for n := 0; n < 20; n++ { // 20 changes inside one spacing
		plane.Publish(machineevents.TopicInventory)
		time.Sleep(spacing / 40)
	}
	time.Sleep(spacing + spacing/2 - time.Since(started))
	if got := refreshes.Load(); got != 2 {
		t.Fatalf("refreshes after 20 changes within %v = %d, want 2 (the first and one folded refresh)", spacing, got)
	}
	time.Sleep(spacing)
	if got := refreshes.Load(); got > 3 {
		t.Fatalf("refreshes kept running after the burst: %d", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v", err)
	}
}

// A change shows locally within the spacing plus one refresh (smoke check inventory-freshness, 30 s): the spacing
// counts from the start of the previous refresh. Counted from its end, a change that arrived just after a slow
// refresh waited the whole spacing and then a whole refresh (40 s on device with a 30 s spacing).
func TestInventoryChangeIsShownWithinTheSpacingOfThePreviousRefreshStart(t *testing.T) {
	const spacing, duration = time.Second, 500 * time.Millisecond
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicInventory)
	refreshed := make(chan time.Time, 8)
	inventory, err := NewInventory(InventoryConfig{Source: slowMachineSource{delay: duration}, Store: store, RefreshInterval: time.Hour,
		RequestTimeout: 5 * time.Second, Events: subscriptions[0], EventRefreshSpacing: spacing, OnRefresh: func(error) { refreshed <- time.Now() }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		t.Fatal("no first refresh")
	}
	changed := time.Now() // just after a refresh finished
	plane.Publish(machineevents.TopicInventory)
	select {
	case shown := <-refreshed:
		if took := shown.Sub(changed); took > spacing+duration/2 {
			t.Fatalf("a change after a refresh was shown after %v, want within the spacing (%v) of the previous refresh's start plus that refresh", took.Round(10*time.Millisecond), spacing)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the change was never refreshed")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v", err)
	}
}

type slowMachineSource struct{ delay time.Duration }

func (s slowMachineSource) ListUserMachines(ctx context.Context) ([]api.UserMachine, error) {
	select {
	case <-time.After(s.delay):
		return []api.UserMachine{{ID: "machine_1", Alias: "studio"}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// On a slow control plane the machine list uses most of a request budget. The local reconcile that
// follows must still get a whole budget; before, it inherited the remainder, timed out, and the
// daemon withdrew every machine route.
func TestInventoryReconcileDoesNotInheritTheListingsTimeBudget(t *testing.T) {
	store, _ := localapi.NewSnapshotStore(nil)
	var reconcileErr error
	inventory, err := NewInventory(InventoryConfig{
		Source: slowMachineSource{delay: 120 * time.Millisecond}, Store: store, RefreshInterval: time.Second, RequestTimeout: 200 * time.Millisecond,
		Clock: time.Now,
		ReconcileMachines: func(ctx context.Context, _ []api.UserMachine) error {
			select {
			case <-time.After(120 * time.Millisecond):
			case <-ctx.Done():
				reconcileErr = ctx.Err()
			}
			return reconcileErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := inventory.Refresh(context.Background()); err != nil || reconcileErr != nil {
		t.Fatalf("refresh err = %v, reconcile err = %v; want both stages inside their own budgets", err, reconcileErr)
	}
}

// The local reconcile asks the server for credentials, browser services and certificates. When it
// failed because the server did not answer, the daemon said "Local machine access is unavailable", which
// sent people to their local settings and made an update treat a slow server as a broken installation
// and roll back at once (lanel-a, 2026-10-23). It reports the control plane instead; any other reconcile
// failure stays local.
func TestInventoryReconcileFailureCausedByTheServerReportsTheControlPlane(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"server deadline":     {fmt.Errorf("refresh authorized local browser services: %w", &url.Error{Op: "Get", URL: "https://api.example.test/v1/machine-services", Err: context.DeadlineExceeded}), "control_plane_unavailable"},
		"local deadline":      {fmt.Errorf("apply local machine access: %w", context.DeadlineExceeded), "local_access_unavailable"},
		"server unavailable":  {fmt.Errorf("coordinator: %w", &api.APIError{Status: 503, Code: "temporarily_unavailable"}), "control_plane_unavailable"},
		"local guard failure": {errors.New("apply local machine access: machine guard refused"), "local_access_unavailable"},
		"server refused":      {&api.APIError{Status: 403, Code: "forbidden"}, "local_access_unavailable"},
		// A peer or local service not answering is not the control plane (hp 2026-10-27: an update rolled back on it).
		"peer deadline": {fmt.Errorf("apply browser routes: %w", &url.Error{Op: "Get", URL: "http://100.64.0.7:38080/health", Err: context.DeadlineExceeded}), "local_access_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := localapi.NewSnapshotStore(nil)
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := NewInventory(InventoryConfig{
				Source:            &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}},
				Store:             store,
				RefreshInterval:   time.Second,
				RequestTimeout:    time.Second,
				ReconcileMachines: func(context.Context, []api.UserMachine) error { return test.err },
			})
			if err != nil {
				t.Fatal(err)
			}
			inventory.setupSince = time.Unix(1, 0) // past start-up: a failure is degraded
			_ = inventory.Refresh(context.Background())
			snapshot, err := store.Snapshot(context.Background())
			if err != nil || snapshot.DaemonState != "degraded" || len(snapshot.Health) != 1 || snapshot.Health[0].Code != test.want || len(snapshot.Machines) != 1 {
				t.Fatalf("snapshot=%#v err=%v, want degraded with %s and the machine kept", snapshot, err, test.want)
			}
		})
	}
}

// A machine without published host keys is shown as publishing (a fresh container still installing, ltc-d7), with
// no cloned-image text; only a refusal the server lists as another machine's keys gives the clone fix (Lane H).
func TestUnpublishedHostKeysSayPublishingAndOnlyARefusalGivesTheCloneFix(t *testing.T) {
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", Online: true, InstallationGeneration: 4, SSHLocalReady: true}
	machine.SSHAuthority.TargetGeneration = 4
	item := mapMachines([]api.UserMachine{machine})[0].Health[0]
	text := item.Title + " " + item.Recovery
	if item.Code != "ssh_host_keys_unpublished" || !strings.HasPrefix(item.Title, "Publishing") || strings.Contains(text, "identical") || strings.Contains(text, "ssh-keygen") {
		t.Fatalf("not yet published: %+v", item)
	}
	machine.SSHAuthority.HostKeyInUse, machine.SSHAuthority.HostKeyInUseBy = true, "build"
	item = mapMachines([]api.UserMachine{machine})[0].Health[0]
	text = item.Title + " " + item.Recovery
	if item.Code != "ssh_host_key_in_use" || !strings.Contains(item.Title, "identical to build's") || !strings.Contains(text, "ssh-keygen -A") {
		t.Fatalf("refused: %+v", item)
	}
	machine.SSHAuthority.HostKeyInUseBy = "" // the other machine is not visible to this account
	if item = mapMachines([]api.UserMachine{machine})[0].Health[0]; !strings.HasSuffix(item.Title, "identical to another machine's") {
		t.Fatalf("refused, other hidden: %+v", item)
	}
}

// Daemons re-listed the account's machines (once per workspace) every 15 s while the server's change
// stream was idle. A healthy, idle stream must mean no machine listing between events.
func TestInventoryMakesNoListingWhileTheChangeStreamIsHealthyAndIdle(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}
	var refreshes atomic.Int32
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, subscriptions, _ := eventstest.Start(t, time.Hour, machineevents.TopicInventory)
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Second, RequestTimeout: time.Second, Events: subscriptions[0], EventFallback: time.Hour, OnRefresh: func(error) { refreshes.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	time.Sleep(3500 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v", err)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("%d refreshes in 3.5 s with a healthy idle stream and a 1 s periodic check, want only the first", got)
	}
}

// While the change stream is unhealthy the periodic refresh backs off with jitter instead of re-listing
// every interval in lockstep across the fleet.
func TestInventoryOutageRefreshBacksOffWithJitter(t *testing.T) {
	interval := 15 * time.Second
	if got := outageRefreshDelay(interval, 0); got != interval {
		t.Fatalf("healthy delay %v, want %v", got, interval)
	}
	for outages, want := range map[int]time.Duration{1: 15 * time.Second, 2: 30 * time.Second, 3: time.Minute, 5: 4 * time.Minute, 6: outageMaximumRefresh, 32: outageMaximumRefresh} {
		seen := map[time.Duration]bool{}
		for range 50 {
			got := outageRefreshDelay(interval, outages)
			if got < want-want/5 || got > want+want/5 {
				t.Fatalf("outage %d: delay %v outside %v ± 20%%", outages, got, want)
			}
			seen[got] = true
		}
		if len(seen) < 2 {
			t.Fatalf("outage %d: no jitter", outages)
		}
	}
}

// pb wait asks its daemon for a refresh every few seconds; requests from local commands are spaced so
// a loop of them cannot turn into a full account listing each time.
func TestRequestedRefreshesFoldWithinTheSpacing(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var at []time.Time
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Hour, RequestTimeout: time.Second, OnRefresh: func(error) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	for range 14 { // 700 ms of requests, inside one spacing
		inventory.RequestRefresh()
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(requestedRefreshSpacing)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 2 {
		t.Fatalf("%d refreshes for the start plus 14 requests within 700 ms, want 2 (the requests folded into one)", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < requestedRefreshSpacing-50*time.Millisecond {
		t.Fatalf("requested refresh %v after the previous one, want at least %v", gap, requestedRefreshSpacing)
	}
}

// A refresh that fails (here the local reconcile, as during the service restarts of pb update) is
// retried on the outage backoff, not left until the event fallback because the change stream is healthy:
// otherwise a daemon stays "Local machine access is unavailable" for minutes after an update.
func TestInventoryRetriesAFailedRefreshWhileTheChangeStreamIsHealthy(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, subscriptions, _ := eventstest.Start(t, time.Hour, machineevents.TopicInventory)
	var reconciles atomic.Int32
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Second, RequestTimeout: time.Second, Events: subscriptions[0], EventFallback: time.Hour,
		ReconcileMachines: func(context.Context, []api.UserMachine) error {
			if reconciles.Add(1) == 1 {
				return errors.New("machine guard is restarting")
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(4 * time.Second)
	for {
		snapshot, err := store.Snapshot(t.Context())
		if err == nil && snapshot.DaemonState == "ready" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon state %q after %d reconciles, 4 s after a failed first refresh with a 1 s interval; want ready on the outage retry", snapshot.DaemonState, reconciles.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A machine revoked a moment ago: its events wait is refused with 401, and the inventory refreshes within a
// second although the event spacing is an hour, so pb status says signed out at once (C036).
func TestInventoryRefreshesAtOnceWhenTheEventWaitIsRefused(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}
	refreshed := make(chan struct{}, 8)
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicInventory)
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Hour, RequestTimeout: time.Second, Events: subscriptions[0], EventRefreshSpacing: time.Hour, OnRefresh: func(error) { refreshed <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = inventory.Run(ctx) }()
	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("no first refresh")
	}
	plane.FailNextWith(1, http.StatusUnauthorized)
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("a refused event wait did not refresh the inventory within a second")
	}
}

// An access change (a machine shared with this account, a grant, the team's private access setting) refreshes the
// machine list within the request spacing, not the event spacing: a member waited 12-31 s to reach a machine just
// shared with them (F9-04, hp 2026-10-27).
func TestInventoryRefreshesAtOnceForAnAccessChange(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}
	refreshed := make(chan struct{}, 8)
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicInventory, machineevents.TopicMachineAccess)
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Hour, RequestTimeout: time.Second,
		Events: subscriptions[0], AccessEvents: subscriptions[1], EventRefreshSpacing: time.Hour, OnRefresh: func(error) { refreshed <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	defer func() { cancel(); <-done }()
	<-refreshed // the first refresh starts the hour-long event spacing
	plane.Publish(machineevents.TopicMachineAccess)
	select {
	case <-refreshed:
	case <-time.After(requestedRefreshSpacing + 2*time.Second):
		t.Fatal("an access change waited for the event spacing")
	}
	// A setting flip publishes inventory and machine_access together: the inventory event arms the hour-long
	// spacing first, and the access change must still not wait for it (F9-04: 9-20 s after a setting flip).
	// An off->on pair: each flip is inventory then machine_access, and each refreshes within the request spacing.
	for _, flip := range []string{"setting off", "setting on"} {
		plane.Publish(machineevents.TopicInventory)
		time.Sleep(200 * time.Millisecond)
		plane.Publish(machineevents.TopicMachineAccess)
		select {
		case <-refreshed:
		case <-time.After(requestedRefreshSpacing + 2*time.Second):
			t.Fatalf("%s: the access change waited for the refresh an inventory event had armed", flip)
		}
	}
}

// F14-03: a daemon that just started and has not finished its first local-access setup (the native listener waits for
// the peer network) reports starting, which a supplied install waits for; past the grace, or after a setup that
// succeeded, a failure is degraded, which fails the install at once.
func TestAFirstLocalAccessFailureAtStartIsStarting(t *testing.T) {
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	failures := []error{errors.New("native listener: network timeout"), nil, errors.New("local access broke")}
	calls := 0
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := NewInventory(InventoryConfig{
		Source: &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}},
		Store:  store, RefreshInterval: time.Second, RequestTimeout: time.Second, Clock: clock,
		ReconcileMachines: func(context.Context, []api.UserMachine) error {
			err := failures[min(calls, len(failures)-1)]
			calls++
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := func(why string, want string) {
		t.Helper()
		_ = inventory.Refresh(context.Background())
		snapshot, err := store.Snapshot(context.Background())
		if err != nil || snapshot.DaemonState != want {
			t.Fatalf("%s: daemon state %q, want %q (err %v)", why, snapshot.DaemonState, want, err)
		}
	}
	state("first setup failed right after start", "starting")
	now = now.Add(time.Second)
	state("setup succeeded", "ready")
	state("a later failure", "degraded")

	inventory.reconciledOnce, inventory.setupSince, calls = false, now.Add(-localAccessStartupGrace), 0
	state("first setup still failing after the grace", "degraded")
}

// A daemon that waited for `pb pair` longer than the grace still reads as starting while its first local-access setup
// after pairing finishes: the grace counts from that first setup, not from daemon start.
func TestTheStartupGraceCountsFromPairing(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	notPaired := machineResult{err: errors.New("waiting for enrollment")}
	paired := machineResult{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}
	inventory, err := NewInventory(InventoryConfig{
		Source: &scriptedMachineSource{results: []machineResult{notPaired, paired}},
		Store:  store, RefreshInterval: time.Second, RequestTimeout: time.Second, Clock: func() time.Time { return now },
		ReconcileMachines: func(context.Context, []api.UserMachine) error { return errors.New("native listener: network timeout") },
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = inventory.Refresh(context.Background())
	now = now.Add(10 * localAccessStartupGrace) // the daemon waited a long time to be paired
	_ = inventory.Refresh(context.Background())
	if snapshot, err := store.Snapshot(context.Background()); err != nil || snapshot.DaemonState != "starting" {
		t.Fatalf("first setup right after pairing: state %q (err %v), want starting", snapshot.DaemonState, err)
	}
	now = now.Add(localAccessStartupGrace)
	_ = inventory.Refresh(context.Background())
	if snapshot, _ := store.Snapshot(context.Background()); snapshot.DaemonState != "degraded" {
		t.Fatalf("setup still failing a grace after pairing: state %q, want degraded", snapshot.DaemonState)
	}
}

// Every degraded readiness names its health item: a machine that just came online, with managed SSH not reporting yet
// and file transfer not observed, says those are still starting (seen on hp: ssh_readiness degraded, health empty).
func TestADegradedReadinessAlwaysNamesItsHealthItem(t *testing.T) {
	machine := api.UserMachine{ID: "machine_1", Alias: "fresh", Online: true, InstallationGeneration: 3,
		SSHAuthority: api.SSHAuthority{TargetGeneration: 3, HostKeyGeneration: 3},
		Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true}}}
	status := mapMachines([]api.UserMachine{machine})[0]
	if status.SSHReadiness != "degraded" || status.TransferReadiness != "degraded" {
		t.Fatalf("fixture: ssh=%s transfer=%s", status.SSHReadiness, status.TransferReadiness)
	}
	if len(status.Health) != 1 || status.Health[0].Code != "machine_services_starting" || status.Health[0].Title != "Still starting on this machine: SSH, file transfer" {
		t.Fatalf("health = %+v", status.Health)
	}
	machine.SSHLocalReady = true
	machine.Capabilities.FileReceive.Observed = true
	if status := mapMachines([]api.UserMachine{machine})[0]; len(status.Health) != 0 || status.SSHReadiness != "ready" {
		t.Fatalf("a ready machine: ssh=%s health=%+v", status.SSHReadiness, status.Health)
	}
}

// A machine list that fails because the API did not answer is the control plane; one that fails in a local step
// (configuration, credentials, alias validation) is named as such, so the update gate does not wait on the server
// for it (hp, 2026-10-27).
func TestInventoryListingFailureNamesTheControlPlaneOnlyForTheAPI(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"API deadline":      {apiDown("/v1/machines"), "control_plane_unavailable"},
		"API unavailable":   {fmt.Errorf("list machines: %w", &api.APIError{Status: 503, Code: "temporarily_unavailable"}), "control_plane_unavailable"},
		"local credentials": {errors.New("read the daemon credential: permission denied"), "machine_list_unavailable"},
		"peer request":      {&url.Error{Op: "Get", URL: "https://peer.example.test/v1/x", Err: context.DeadlineExceeded}, "machine_list_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := localapi.NewSnapshotStore(nil)
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := NewInventory(InventoryConfig{Source: &scriptedMachineSource{results: []machineResult{{err: test.err}}}, Store: store, RefreshInterval: time.Second, RequestTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_ = inventory.Refresh(context.Background())
			snapshot, err := store.Snapshot(context.Background())
			if err != nil || snapshot.DaemonState != "degraded" || len(snapshot.Health) != 1 || snapshot.Health[0].Code != test.want {
				t.Fatalf("snapshot=%#v err=%v, want degraded with %s", snapshot, err, test.want)
			}
		})
	}
}
