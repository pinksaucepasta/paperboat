package api

import (
	"context"
	"net/http"
	"net/url"
)

// RelayLatency is the connect time (milliseconds) a machine measured to one relay.
type RelayLatency struct {
	NodeID    string `json:"node_id"`
	RTTMillis int64  `json:"rtt_ms"`
}

// ReportRelayLatency tells the server how long this machine took to reach each relay. The numbers
// are display data for the console and CLI.
func (c *Client) ReportRelayLatency(ctx context.Context, machineID string, relays []RelayLatency) error {
	body := struct {
		Relays []RelayLatency `json:"relays"`
	}{Relays: relays}
	return c.do(ctx, http.MethodPut, "/v1/machines/"+url.PathEscape(machineID)+"/network-latency", body, nil)
}

// MachineLatency is the best connect time a machine measured to one region's edges or relays: the server
// folds a region's nodes into one row per role and never names a node.
type MachineLatency struct {
	Region     string `json:"region"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	SelfHosted bool   `json:"self_hosted"`
	RTTMillis  int64  `json:"rtt_ms"`
}

// MachineLatencies lists the latest best connect times (under an hour old) a machine reported to each region's edges and relays.
func (c *Client) MachineLatencies(ctx context.Context, machineID string) ([]MachineLatency, error) {
	var out struct {
		Items []MachineLatency `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(machineID)+"/network-latency", nil, &out)
	return out.Items, err
}
