//go:build darwin || linux

package localdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// While the latest network refresh withheld relays at the plan limit, the daemon snapshot names it;
// the wrapper still exposes the inventory's refresh to the local API (pb login refreshes through it).
func TestSnapshotNamesTheRelayPlanLimit(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{}}}}
	inventory, _ := newTestInventory(t, source, func() time.Time { return now })
	var denial *api.APIError
	var wrapped localapi.SnapshotSource = snapshotSource{Inventory: inventory, denial: func() *api.APIError { return denial }}
	if _, ok := wrapped.(interface{ Refresh(context.Context) error }); !ok {
		t.Fatal("the wrapper hides the inventory refresh")
	}
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := wrapped.Snapshot(context.Background())
	if err != nil || len(snapshot.Health) != 0 {
		t.Fatalf("without a denial: health=%+v err=%v", snapshot.Health, err)
	}
	denial = &api.APIError{Status: http.StatusConflict, Code: localapi.RelayQuotaCode, Message: "Hosted relay bandwidth is used up."}
	snapshot, err = wrapped.Snapshot(context.Background())
	if err != nil || len(snapshot.Health) != 1 || snapshot.Health[0].Code != localapi.RelayQuotaCode || snapshot.Health[0].Title != denial.Message || snapshot.Validate() != nil {
		t.Fatalf("with a denial: health=%+v err=%v", snapshot.Health, err)
	}
}

// F21-06: the native runtime's paths and relay failover reach the daemon snapshot that pb status, pb doctor
// and the bug report read: a machine nobody reported gets its live path and relay region, and a relay
// failover or an unusable relay is a health item with its reason and probe failures.
func TestNativeTransportReachesTheSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	machines := []api.UserMachine{
		{ID: "machine_direct", Alias: "a", State: "online", Online: true, InstallationGeneration: 1},
		{ID: "machine_relay", Alias: "b", State: "online", Online: true, InstallationGeneration: 1},
		{ID: "machine_idle", Alias: "c", State: "online", Online: true, InstallationGeneration: 1},
	}
	inventory, _ := newTestInventory(t, &scriptedMachineSource{results: []machineResult{{machines: machines}}}, func() time.Time { return now })
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	native := tunnel.NativeTransport{
		Paths: map[string]string{"machine_direct": "direct", "machine_relay": "regional_relay"},
		Relay: tailnet.RegionalStatus{NodeID: "relay_fra_2", Region: "eu-central", LocalTransport: "derp_quic", Reason: "selected_node_unreachable", ProbeFailures: map[string]string{"relay_fra_1": "timeout"}},
	}
	source := snapshotSource{Inventory: inventory, transport: func() tunnel.NativeTransport { return native }}
	snapshot, err := source.Snapshot(context.Background())
	if err != nil || snapshot.Validate() != nil {
		t.Fatalf("snapshot err=%v validate=%v", err, snapshot.Validate())
	}
	paths := map[string]string{}
	for _, m := range snapshot.Machines {
		paths[m.ID] = m.SelectedPath + "/" + m.RelayRegion
	}
	if paths["machine_direct"] != "direct/" || paths["machine_relay"] != "relay/eu-central" || paths["machine_idle"] != "none/" {
		t.Fatalf("paths = %v", paths)
	}
	// The home relay this machine advertises (the selected one) is in pb status --json by node id, with its region label.
	if encoded, _ := json.Marshal(snapshot.HomeRelay); string(encoded) != `{"node_id":"relay_fra_2","region":"eu-central"}` {
		t.Fatalf("home relay = %s", encoded)
	}
	if len(snapshot.Health) != 1 || snapshot.Health[0].Code != RelayFailoverCode || snapshot.Health[0].Severity != "info" ||
		snapshot.Health[0].Title != "The relay failed over to eu-central: the previous relay became unreachable; probe failures: relay_fra_1 timeout" {
		t.Fatalf("failover health = %+v", snapshot.Health)
	}

	// This engine cannot use any relay: a warning with the reason; WebSocket relay transport is reported as wss.
	native.Relay = tailnet.RegionalStatus{Reason: "regional_admission_denied", ProbeFailures: map[string]string{"relay_b": "udp_blocked", "relay_a": "timeout"}}
	snapshot, _ = source.Snapshot(context.Background())
	if snapshot.HomeRelay != nil {
		t.Fatalf("no relay is selected, yet the home relay is %+v", snapshot.HomeRelay)
	}
	if len(snapshot.Health) != 1 || snapshot.Health[0].Severity != "warning" || snapshot.Validate() != nil ||
		snapshot.Health[0].Title != "No relay is usable: the relays refused admission; probe failures: relay_a timeout, relay_b udp_blocked" {
		t.Fatalf("unusable relay health = %+v", snapshot.Health)
	}
	// The network itself is down (no interface up): after 5 s that one item shows and the relay warning that follows
	// from it does not; when the network returns, it clears (Tailscale's NetworkStatusWarnable).
	up, clock := false, now
	source.network = &networkStatus{up: func() bool { return up }, now: func() time.Time { return clock }}
	if snapshot, _ = source.Snapshot(context.Background()); len(snapshot.Health) != 1 || snapshot.Health[0].Code != RelayFailoverCode {
		t.Fatalf("network down for 0 s already shown: %+v", snapshot.Health)
	}
	clock = clock.Add(networkDownVisible)
	if snapshot, _ = source.Snapshot(context.Background()); len(snapshot.Health) != 1 || snapshot.Health[0].Code != NetworkDownCode || snapshot.Validate() != nil {
		t.Fatalf("network down health = %+v", snapshot.Health)
	}
	up = true
	if snapshot, _ = source.Snapshot(context.Background()); len(snapshot.Health) != 1 || snapshot.Health[0].Code != RelayFailoverCode {
		t.Fatalf("network back, health = %+v", snapshot.Health)
	}
	source.network = nil
	// One pair without a common reachable relay is not a health item; a dial over it names its cause.
	for _, reason := range []string{"no_mutually_reachable_node", "pair_authority_expired", "promotion_not_acknowledged"} {
		native.Relay = tailnet.RegionalStatus{Reason: reason, ProbeFailures: map[string]string{"relay_a": "retry_backoff"}}
		if snapshot, _ = source.Snapshot(context.Background()); len(snapshot.Health) != 0 {
			t.Fatalf("pair reason %s added health %+v", reason, snapshot.Health)
		}
	}
	native.Relay = tailnet.RegionalStatus{NodeID: "relay_fra_2", Region: "eu-central", LocalTransport: "derp_wss", Reason: "healthy_choice_retained"}
	snapshot, _ = source.Snapshot(context.Background())
	for _, m := range snapshot.Machines {
		if m.ID == "machine_relay" && (m.SelectedPath != "wss" || m.RelayRegion != "eu-central") {
			t.Fatalf("wss relay = %s/%s", m.SelectedPath, m.RelayRegion)
		}
	}
	if len(snapshot.Health) != 0 {
		t.Fatalf("a healthy relay adds health %+v", snapshot.Health)
	}
}

// pb connect labels the path from the local API's snapshot watch; the daemon's source must serve it (it
// answered 501 "snapshot watch is unavailable" before) with the same native path as Snapshot.
func TestSnapshotSourceServesTheWatch(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	machines := []api.UserMachine{{ID: "machine_direct", Alias: "a", State: "online", Online: true, InstallationGeneration: 1}}
	inventory, _ := newTestInventory(t, &scriptedMachineSource{results: []machineResult{{machines: machines}}}, func() time.Time { return now })
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	native := tunnel.NativeTransport{Paths: map[string]string{"machine_direct": "direct"}}
	var source localapi.SnapshotSource = snapshotSource{Inventory: inventory, transport: func() tunnel.NativeTransport { return native }}
	watcher, ok := source.(localapi.SnapshotWatcher)
	if !ok {
		t.Fatal("the daemon snapshot source does not serve the snapshot watch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := watcher.Watch(ctx, 0)
	if err != nil || len(snapshot.Machines) != 1 || snapshot.Machines[0].SelectedPath != "direct" {
		t.Fatalf("watch = %+v err=%v", snapshot.Machines, err)
	}
}

// A failing daemon step is an error health item naming the step and its cause until it recovers; while the
// control plane is unreachable only that root cause shows (the updater's gate reads it alone).
func TestFailingStepsReachTheSnapshotUntilTheyRecover(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	inventory, _ := newTestInventory(t, &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{}}}}, func() time.Time { return now })
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	steps := health.New("test", nil, nil)
	source := snapshotSource{Inventory: inventory, steps: steps}
	steps.Fail(StepRegisterNetwork, errors.New("dial tcp: connection refused"), 4*time.Second)
	snapshot, err := source.Snapshot(context.Background())
	if err != nil || len(snapshot.Health) != 1 || snapshot.Validate() != nil {
		t.Fatalf("failing step: health=%+v err=%v", snapshot.Health, err)
	}
	if item := snapshot.Health[0]; item.Code != StepRegisterNetwork || item.Severity != "error" ||
		!strings.Contains(item.Title, "connection refused") || !strings.Contains(item.Title, "retrying in 4s") {
		t.Fatalf("failing step item = %+v", item)
	}
	steps.Set(StepRegisterNetwork, health.Ready, "", 0)
	if snapshot, err = source.Snapshot(context.Background()); err != nil || len(snapshot.Health) != 0 {
		t.Fatalf("recovered step: health=%+v err=%v", snapshot.Health, err)
	}
	steps.Fail(StepMachineServices, errors.New("server unavailable"), time.Second)
	outage := localapi.Snapshot{Health: []localapi.HealthItem{{Code: "control_plane_unavailable"}}}
	applySteps(&outage, steps)
	if len(outage.Health) != 1 {
		t.Fatalf("step shown behind the control plane outage: %+v", outage.Health)
	}
}

// pbmac 2026-11-05: pb status said Path none for a machine every connection to which failed, and pb doctor said
// healthy. A machine whose last dial failed is a no_path error health item; an idle machine is not.
func TestFailedDialMakesANoPathHealthItem(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 20, 0, 0, time.UTC)
	machines := []api.UserMachine{
		{ID: "machine_failed", Alias: "macl", State: "online", Online: true, InstallationGeneration: 1},
		{ID: "machine_idle", Alias: "idle", State: "online", Online: true, InstallationGeneration: 1},
	}
	inventory, _ := newTestInventory(t, &scriptedMachineSource{results: []machineResult{{machines: machines}}}, func() time.Time { return now })
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	native := tunnel.NativeTransport{NoPath: map[string]time.Time{"machine_failed": now.Add(-time.Second)}}
	source := snapshotSource{Inventory: inventory, transport: func() tunnel.NativeTransport { return native }}
	snapshot, err := source.Snapshot(context.Background())
	if err != nil || snapshot.Validate() != nil {
		t.Fatalf("snapshot err=%v validate=%v", err, snapshot.Validate())
	}
	noPath := func(m localapi.MachineStatus) *localapi.HealthItem {
		for index := range m.Health {
			if m.Health[index].Code == localapi.NoPathCode {
				return &m.Health[index]
			}
		}
		return nil
	}
	for _, m := range snapshot.Machines {
		switch item := noPath(m); m.ID {
		case "machine_failed":
			if item == nil || item.Severity != "error" || m.SelectedPath != "none" || item.Title != "No path to macl: the last connection attempt failed" {
				t.Fatalf("failed machine = %+v", m)
			}
		case "machine_idle":
			if item != nil {
				t.Fatalf("idle machine got %+v", item)
			}
		}
	}
	// A path in use wins over an older failure.
	native.Paths = map[string]string{"machine_failed": "direct"}
	snapshot, _ = source.Snapshot(context.Background())
	for _, m := range snapshot.Machines {
		if m.ID == "machine_failed" && (noPath(m) != nil || m.SelectedPath != "direct") {
			t.Fatalf("machine with a path = %+v", m)
		}
	}
}
