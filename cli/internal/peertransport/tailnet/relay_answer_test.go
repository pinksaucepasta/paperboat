package tailnet

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/net/stun"
)

// F21-49: a network that returns without a link change is noticed from the first relay STUN answer, within about one
// probe interval, and the API is not asked for it.
func TestRelayAnswerIsNoticedSoonAfterTheNetworkReturns(t *testing.T) {
	relay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	var up atomic.Bool
	var asked atomic.Int32
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, peer, err := relay.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			asked.Add(1)
			tx, err := stun.ParseBindingRequest(buffer[:n])
			if err != nil || !up.Load() { // the network still drops everything
				continue
			}
			relay.WriteToUDP(stun.Response(tx, netip.MustParseAddrPort(peer.String())), peer)
		}
	}()
	a, _, _, _ := networkTestAuthority(t)
	a.mu.Lock()
	a.regional = &RegionalCandidates{Nodes: []RegionalNode{{NodeID: "node_a", EndpointHost: "127.0.0.1", EndpointTCPPort: uint16(relay.LocalAddr().(*net.UDPAddr).Port)}}}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- a.awaitRelayAnswer(ctx) }()
	time.Sleep(5 * time.Second)
	select {
	case <-result:
		t.Fatal("reported an answer while the relay was silent")
	default:
	}
	if asked.Load() < 2 {
		t.Fatalf("asked the relay %d times in 5 s", asked.Load())
	}
	up.Store(true)
	start := time.Now()
	if !<-result {
		t.Fatal("no answer noticed")
	}
	if took := time.Since(start); took > 2*relayAnswerInterval {
		t.Fatalf("noticed %v after the relay answered", took)
	}
}
