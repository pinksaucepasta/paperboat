//go:build darwin || linux

package localdaemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
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
	if len(snapshot.Health) != 1 || snapshot.Health[0].Code != RelayFailoverCode || snapshot.Health[0].Severity != "info" ||
		snapshot.Health[0].Title != "The relay failed over to eu-central: the previous relay became unreachable; probe failures: relay_fra_1 timeout" {
		t.Fatalf("failover health = %+v", snapshot.Health)
	}

	// This engine cannot use any relay: a warning with the reason; WebSocket relay transport is reported as wss.
	native.Relay = tailnet.RegionalStatus{Reason: "regional_admission_denied", ProbeFailures: map[string]string{"relay_b": "udp_blocked", "relay_a": "timeout"}}
	snapshot, _ = source.Snapshot(context.Background())
	if len(snapshot.Health) != 1 || snapshot.Health[0].Severity != "warning" || snapshot.Validate() != nil ||
		snapshot.Health[0].Title != "No relay is usable: the relays refused admission; probe failures: relay_a timeout, relay_b udp_blocked" {
		t.Fatalf("unusable relay health = %+v", snapshot.Health)
	}
	// The network itself is down: that one item shows, the relay warning that follows from it does not.
	native.NetworkDown = true
	snapshot, _ = source.Snapshot(context.Background())
	if len(snapshot.Health) != 1 || snapshot.Health[0].Code != NetworkDownCode || snapshot.Validate() != nil {
		t.Fatalf("network down health = %+v", snapshot.Health)
	}
	native.NetworkDown = false
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
