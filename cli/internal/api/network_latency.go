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
