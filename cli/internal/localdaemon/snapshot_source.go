package localdaemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/networkmonitor"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// snapshotSource adds what only the daemon's live runtime knows to the stored snapshot, at read time:
//   - the hosted relay plan limit while the latest network refresh withheld relays for it. A stream the
//     relay cut at the limit ends like any lost connection; this lets pb status and the failed command name
//     the limit and the Billing action instead;
//   - the native transport (F21-06): the path to each machine with an active native connection and its relay
//     region, and a relay_failover health item with the reason and probe failures when the regional relay
//     failed over or none is usable, so pb status, pb doctor and the bug report show them.
//
// It embeds the inventory so the local API still finds its refresh method.
type snapshotSource struct {
	*Inventory
	denial    func() *api.APIError
	transport func() tunnel.NativeTransport
	network   *networkStatus
}

// networkDownVisible is how long no interface may be up before pb status says so (Tailscale's
// NetworkStatusWarnable TimeToVisible, health/warnings.go).
const networkDownVisible = 5 * time.Second

// networkStatus is Tailscale's NetworkStatusWarnable for the daemon: down once no interface has been up for
// networkDownVisible.
type networkStatus struct {
	up        func() bool
	now       func() time.Time
	mu        sync.Mutex
	downSince time.Time
}

// startNetworkStatus watches interface state; a network change also refreshes from the control plane at once,
// instead of waiting for the outage backoff (Tailscale re-polls control on a link change).
func startNetworkStatus(refresh func()) (*networkStatus, func(), error) {
	watch, err := networkmonitor.New(func(networkmonitor.Event) { refresh() })
	if err != nil {
		return nil, nil, err
	}
	if err := watch.Start(); err != nil {
		_ = watch.Close()
		return nil, nil, err
	}
	return &networkStatus{up: watch.AnyInterfaceUp, now: time.Now}, func() { _ = watch.Close() }, nil
}

func (n *networkStatus) down() bool {
	if n == nil || n.up == nil {
		return false
	}
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.up() {
		n.downSince = time.Time{}
		return false
	}
	if n.downSince.IsZero() {
		n.downSince = now
	}
	return now.Sub(n.downSince) >= networkDownVisible
}

func (s snapshotSource) Snapshot(ctx context.Context) (localapi.Snapshot, error) {
	snapshot, err := s.Inventory.Snapshot(ctx)
	if err != nil {
		return snapshot, err
	}
	return s.decorate(snapshot), nil
}

// Watch is the local API's snapshot watch (pb connect's path label follows it): the inventory's next
// snapshot with the same additions as Snapshot.
func (s snapshotSource) Watch(ctx context.Context, after uint64) (localapi.Snapshot, error) {
	snapshot, err := s.Inventory.store.Watch(ctx, after)
	if err != nil {
		return snapshot, err
	}
	return s.decorate(snapshot), nil
}

func (s snapshotSource) decorate(snapshot localapi.Snapshot) localapi.Snapshot {
	if s.transport != nil {
		applyNativeTransport(&snapshot, s.transport())
	}
	if s.network.down() {
		hideBehindNetworkDown(&snapshot)
	}
	if s.denial == nil {
		return snapshot
	}
	if denial := s.denial(); denial != nil && denial.Code == localapi.RelayQuotaCode {
		snapshot.Health = append(snapshot.Health, localapi.HealthItem{Code: localapi.RelayQuotaCode, Severity: "warning", Title: denial.Message, Recovery: RelayQuotaRecovery, ETag: localapi.RelayQuotaCode})
	}
	return snapshot
}

// RelayQuotaRecovery is the action for an exhausted hosted relay allowance.
const RelayQuotaRecovery = "Add bandwidth or upgrade under Billing in the Paperboat console, or wait for the next allowance period. Direct connections keep working."

// RelayFailoverCode is the health item for a regional relay failover or an unusable relay.
const RelayFailoverCode = "relay_failover"

// maxProbeFailuresShown bounds the probe failures named in the health item.
const maxProbeFailuresShown = 8

// applyNativeTransport fills the path of machines no local client reported (their selected path is "none")
// from the native runtime, and adds the relay_failover health item.
func applyNativeTransport(snapshot *localapi.Snapshot, native tunnel.NativeTransport) {
	relayPath := "relay"
	if native.Relay.LocalTransport == "derp_wss" {
		relayPath = "wss"
	}
	for index := range snapshot.Machines {
		machine := &snapshot.Machines[index]
		if machine.SelectedPath != "none" || machine.ActiveConsumers != 0 {
			continue
		}
		switch native.Paths[machine.ID] {
		case "direct":
			machine.SelectedPath, machine.RelayRegion = "direct", ""
		case "peer_relay":
			machine.SelectedPath, machine.RelayRegion = "relay", ""
		case "regional_relay":
			machine.SelectedPath, machine.RelayRegion = relayPath, ""
			if localapi.ValidRelayRegion(native.Relay.Region) {
				machine.RelayRegion = native.Relay.Region
			}
		}
	}
	if item, ok := relayFailoverHealth(native.Relay); ok {
		snapshot.Health = append(snapshot.Health, item)
	}
}

// NetworkDownCode is the health item for this computer's network being down.
const NetworkDownCode = "network_down"

// hideBehindNetworkDown shows only the root cause while the network is down: the warnings that follow from it
// (relay, control plane) are dropped, as Tailscale's warnables DependsOn NetworkStatusWarnable
// (health/warnings.go, health/state.go isEffectivelyHealthyLocked).
func hideBehindNetworkDown(snapshot *localapi.Snapshot) {
	kept := snapshot.Health[:0]
	for _, item := range snapshot.Health {
		if item.Code != RelayFailoverCode && item.Code != "control_plane_unavailable" {
			kept = append(kept, item)
		}
	}
	snapshot.Health = append(kept, localapi.HealthItem{Code: NetworkDownCode, ETag: NetworkDownCode, Severity: "warning",
		Title: "This computer's network is down", Recovery: "Check the Wi-Fi or cable. Paperboat reconnects on its own when the network returns."})
}

func relayFailoverHealth(status tailnet.RegionalStatus) (localapi.HealthItem, bool) {
	item := localapi.HealthItem{Code: RelayFailoverCode, ETag: RelayFailoverCode}
	switch {
	case status.NodeID == "" && engineRelayFailure(status.Reason):
		item.Severity = "warning"
		item.Title = "No relay is usable: " + relayFailoverReason(status.Reason)
		item.Recovery = "Direct connections keep working. Paperboat retries the relays automatically; check that UDP and HTTPS to the relays are allowed, then run pb doctor."
	case status.Reason == "selected_node_unreachable":
		item.Severity = "info"
		item.Title = "The relay failed over"
		if status.Region != "" {
			item.Title += " to " + status.Region
		}
		item.Title += ": the previous relay became unreachable"
		item.Recovery = "No action is needed; Paperboat moved the relay path automatically."
	default:
		return localapi.HealthItem{}, false
	}
	if failures := probeFailureText(status.ProbeFailures); failures != "" {
		item.Title += "; probe failures: " + failures
	}
	if len(item.Title) > 512 {
		item.Title = item.Title[:509] + "..."
	}
	return item, true
}

// engineRelayFailure reports whether reason says this engine itself cannot use any relay. Per-pair
// reachability (a pair with no common reachable relay, an expired pair authority) is not a health item: a
// dial that needs that pair names its cause. Prior art: Tailscale warns only about the node's own home-relay
// connection (health/warnings.go noDERPConnectionWarnable); pb status warned about pairs not in use (hp
// 2026-10-27).
func engineRelayFailure(reason string) bool {
	return reason == "regional_admission_denied" || reason == "no_authorized_common_node"
}

func relayFailoverReason(reason string) string {
	switch reason {
	case "no_authorized_common_node":
		return "no relay is authorized for both machines"
	case "pair_authority_expired":
		return "the relay authorization expired"
	case "regional_admission_denied":
		return "the relays refused admission"
	case "no_mutually_reachable_node":
		return "no relay is reachable from both machines"
	case "promotion_not_acknowledged":
		return "the chosen relay did not confirm the switch"
	case "stale_observation":
		return "the relay measurements are out of date"
	default:
		return reason
	}
}

func probeFailureText(failures map[string]string) string {
	nodes := make([]string, 0, len(failures))
	for node := range failures {
		nodes = append(nodes, node)
	}
	slices.Sort(nodes)
	parts := make([]string, 0, min(len(nodes), maxProbeFailuresShown)+1)
	for i, node := range nodes {
		if i == maxProbeFailuresShown {
			parts = append(parts, fmt.Sprintf("and %d more", len(nodes)-i))
			break
		}
		parts = append(parts, node+" "+failures[node])
	}
	return strings.Join(parts, ", ")
}
