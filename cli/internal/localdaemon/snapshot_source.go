package localdaemon

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
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
// It embeds the inventory so the local API still finds its refresh and watch methods.
type snapshotSource struct {
	*Inventory
	denial    func() *api.APIError
	transport func() tunnel.NativeTransport
}

func (s snapshotSource) Snapshot(ctx context.Context) (localapi.Snapshot, error) {
	snapshot, err := s.Inventory.Snapshot(ctx)
	if err != nil {
		return snapshot, err
	}
	if s.transport != nil {
		applyNativeTransport(&snapshot, s.transport())
	}
	if s.denial == nil {
		return snapshot, nil
	}
	if denial := s.denial(); denial != nil && denial.Code == localapi.RelayQuotaCode {
		snapshot.Health = append(snapshot.Health, localapi.HealthItem{Code: localapi.RelayQuotaCode, Severity: "warning", Title: denial.Message, Recovery: RelayQuotaRecovery, ETag: localapi.RelayQuotaCode})
	}
	return snapshot, nil
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

func relayFailoverHealth(status tailnet.RegionalStatus) (localapi.HealthItem, bool) {
	item := localapi.HealthItem{Code: RelayFailoverCode, ETag: RelayFailoverCode}
	switch {
	case status.NodeID == "" && status.Reason != "":
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
