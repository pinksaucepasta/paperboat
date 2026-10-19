package daemoncmd

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/internal/netlatency"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// newRelayLatencyReporter measures this machine's connect time to the relays the account can use
// and reports it for the console's Network section. It runs for the daemon's lifetime and a failed
// round never affects local access.
func newRelayLatencyReporter(serverURL, machineID string, transport http.RoundTripper, credential func(context.Context) (config.Credential, error), relays func(context.Context) ([]tailnet.RegionalNode, error)) *netlatency.Reporter {
	return &netlatency.Reporter{
		Targets: func(ctx context.Context) ([]netlatency.Target, error) {
			nodes, err := relays(ctx)
			if err != nil {
				return nil, err
			}
			return relayTargets(nodes), nil
		},
		Report: func(ctx context.Context, measurements []netlatency.Measurement) error {
			token, err := credential(ctx)
			if err != nil {
				return err
			}
			report := make([]api.RelayLatency, len(measurements))
			for i, measurement := range measurements {
				report[i] = api.RelayLatency{NodeID: measurement.NodeID, RTTMillis: measurement.RTTMillis}
			}
			return api.New(serverURL, token, &http.Client{Transport: transport}).ReportRelayLatency(ctx, machineID, report)
		},
		OnFailure: func(stage string, err error) {
			diagnosticlog.TryInfo("relay latency report skipped", "stage", stage)
		},
	}
}

// relayTargets lists the relays that advertise a TCP endpoint, once each.
func relayTargets(nodes []tailnet.RegionalNode) []netlatency.Target {
	targets := make([]netlatency.Target, 0, len(nodes))
	for _, node := range nodes {
		if !slices.Contains(node.Roles, "relay") || node.NodeID == "" || node.EndpointHost == "" || node.EndpointTCPPort == 0 {
			continue
		}
		targets = append(targets, netlatency.Target{NodeID: node.NodeID, Address: net.JoinHostPort(node.EndpointHost, strconv.Itoa(int(node.EndpointTCPPort)))})
	}
	return targets
}
