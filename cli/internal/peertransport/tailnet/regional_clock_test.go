package tailnet

import (
	"testing"
	"time"
)

// X-27: a machine whose clock is seconds behind the server's (below serverclock.Threshold, so not corrected) judged a
// relay observed at the set's issue time as observed in the future and selected no relay.
func TestEligibleToleratesAClockBehindTheServer(t *testing.T) {
	server := time.Now()
	node := func(observed time.Time) RegionalNode {
		return RegionalNode{NodeID: "relay_clock", NodeGeneration: 1, ProcessEpoch: "epoch_clock", Region: "test", FailureDomain: "rack",
			Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443,
			State: "ready", ObservedAt: observed.Unix(), CapacityObservedAt: observed.Unix(), ExpiresAt: server.Add(time.Minute).Unix(), CapacityLimit: 10}
	}
	for name, tc := range map[string]struct {
		behind   time.Duration
		observed time.Time
		want     int
	}{
		"clock 5 s behind":               {5 * time.Second, server, 1},
		"clock 59 s behind":              {59 * time.Second, server, 1},
		"observed an hour in the future": {0, server.Add(time.Hour), 0},
		"clock on time":                  {0, server, 1},
	} {
		t.Run(name, func(t *testing.T) {
			c := &RegionalCandidates{IssuedAt: server.Unix(), ExpiresAt: server.Add(time.Minute).Unix(), Nodes: []RegionalNode{node(tc.observed)}}
			nodes, _, err := eligible(c, "relay", "derp_quic", nil, server.Add(-tc.behind))
			if err != nil || len(nodes) != tc.want {
				t.Fatalf("eligible relays = %d (err %v), want %d", len(nodes), err, tc.want)
			}
		})
	}
}
