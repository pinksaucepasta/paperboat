package mesh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// hop-89: a Windows machine restarted after an update and homed on the other relay of the account's pair. hp kept
// sending its handshake to the relay it remembered, which the restarted machine was no longer on, so every exec
// failed until something else moved hp. An unanswered handshake now also asks on the other relays (connecting to
// them when needed), and the region the answer arrives on is recorded.
func TestPeerRestartedOnTheOtherRelayIsReached(t *testing.T) {
	envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	t.Cleanup(func() { envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "") })
	one := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	two := relayRegionCopy(integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1], 2)
	testPeerMovedRelay(t, one, two, 4*time.Second)
}

func relayRegionCopy(r *tailcfg.DERPRegion, id tailcfg.DERPRegionID) *tailcfg.DERPRegion {
	c := *r
	c.RegionID, c.RegionCode, c.Nodes = id, "two", nil
	for _, n := range r.Nodes {
		node := *n
		node.RegionID, node.Name = id, "two"
		c.Nodes = append(c.Nodes, &node)
	}
	return &c
}

func testPeerMovedRelay(t *testing.T, one, two *tailcfg.DERPRegion, within time.Duration) {
	clientKey, hostKey := key.NewNode(), key.NewNode()
	clientAddr := netip.MustParseAddr("fd7a:115c:a1e0::40")
	hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::41")
	started := 0
	start := func(k key.NodePrivate, addr netip.Addr, peer key.NodePublic, peerAddr netip.Addr, regions ...*tailcfg.DERPRegion) *Server {
		t.Helper()
		s := &Server{Key: k, LocalAddr: addr, AllowedPeers: map[key.NodePublic]netip.Addr{peer: peerAddr},
			ServedUDPPorts: []filter.PortRange{{First: 443, Last: 443}}, OnUDP: func(uint16) func(ConnPacketConn) {
				return func(c ConnPacketConn) {
					defer c.Close()
					var buffer [64]byte
					for {
						n, err := c.Read(buffer[:])
						if err != nil {
							return
						}
						if _, err := c.Write(buffer[:n]); err != nil {
							return
						}
					}
				}
			}}
		if err := s.SetRelayRegions(regions); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		// A listener connects to its home relay (Listen -> PrepareRegional); it is reachable only there.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := s.PrepareRelay(ctx, regions[0].RegionID); err != nil {
			t.Fatal(err)
		}
		started++
		return s
	}
	// UDP is off (relay only); a signed candidate per start models the restarted peer's new endpoints.
	contact := func(to, from *Server) {
		t.Helper()
		c := PeerContact{Disco: from.lb.discoPublic(),
			Endpoints: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("192.0.2.20"), uint16(40000+started))}}
		if err := to.SetPeerContacts(map[key.NodePublic]PeerContact{from.Key.Public(): c}); err != nil {
			t.Fatal(err)
		}
	}
	client := start(clientKey, clientAddr, hostKey.Public(), hostAddr, one, two)
	host := start(hostKey, hostAddr, clientKey.Public(), clientAddr, one)
	contact(client, host)
	contact(host, client)
	restartExchange(t, host, client) // the client learns the host's relay from its handshake
	restartExchange(t, client, host)
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	host = start(hostKey, hostAddr, clientKey.Public(), clientAddr, two) // same identity, now on the other relay only
	contact(host, client)
	contact(client, host)
	begin := time.Now()
	restartExchange(t, client, host)
	if took := time.Since(begin); took > within {
		t.Fatalf("reached the moved peer after %v, want within %v", took, within)
	}
}
