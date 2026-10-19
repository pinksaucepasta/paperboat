package daemoncmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/netlatency"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

func TestRelayTargetsKeepOnlyRelaysWithATCPEndpoint(t *testing.T) {
	targets := relayTargets([]tailnet.RegionalNode{
		{NodeID: "node_relay", Roles: []string{"relay"}, EndpointHost: "relay.example.test", EndpointTCPPort: 443},
		{NodeID: "node_edge", Roles: []string{"edge"}, EndpointHost: "edge.example.test", EndpointTCPPort: 443},
		{NodeID: "node_both", Roles: []string{"edge", "relay"}, EndpointHost: "2001:db8::1", EndpointTCPPort: 8443},
		{NodeID: "node_no_port", Roles: []string{"relay"}, EndpointHost: "relay2.example.test"},
		{NodeID: "node_no_host", Roles: []string{"relay"}, EndpointTCPPort: 443},
	})
	if len(targets) != 2 || targets[0] != (netlatency.Target{NodeID: "node_relay", Address: "relay.example.test:443"}) || targets[1] != (netlatency.Target{NodeID: "node_both", Address: "[2001:db8::1]:8443"}) {
		t.Fatalf("targets=%+v", targets)
	}
}

func TestRelayLatencyReporterPutsMeasurementsForThisMachine(t *testing.T) {
	type put struct {
		Path, Auth string
		Body       struct {
			Relays []struct {
				NodeID string `json:"node_id"`
				RTT    int64  `json:"rtt_ms"`
			} `json:"relays"`
		}
	}
	seen := make(chan put, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got put
		got.Path, got.Auth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got.Body)
		seen <- got
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	reporter := newRelayLatencyReporter(server.URL, "machine_1", server.Client().Transport,
		func(context.Context) (config.Credential, error) { return config.Credential{AccessToken: "token"}, nil },
		func(context.Context) ([]tailnet.RegionalNode, error) { return nil, nil })
	if err := reporter.Report(t.Context(), []netlatency.Measurement{{NodeID: "node_relay", RTTMillis: 23}}); err != nil {
		t.Fatal(err)
	}
	got := <-seen
	if got.Path != "PUT /v1/machines/machine_1/network-latency" || got.Auth != "Bearer token" || len(got.Body.Relays) != 1 || got.Body.Relays[0].NodeID != "node_relay" || got.Body.Relays[0].RTT != 23 {
		t.Fatalf("request=%+v", got)
	}
}
