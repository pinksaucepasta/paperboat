package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
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

// MachineLatency is one fresh measurement the server holds for a machine: connect time to an edge or relay.
type MachineLatency struct {
	NodeID     string    `json:"node_id"`
	Role       string    `json:"role"`
	Region     string    `json:"region"`
	SelfHosted bool      `json:"self_hosted"`
	RTTMillis  int64     `json:"rtt_ms"`
	MeasuredAt time.Time `json:"measured_at"`
}

// MachineLatencies lists the latest connect times (under an hour old) a machine reported to edges and relays.
func (c *Client) MachineLatencies(ctx context.Context, machineID string) ([]MachineLatency, error) {
	var out struct {
		Items []MachineLatency `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(machineID)+"/network-latency", nil, &out)
	return out.Items, err
}
