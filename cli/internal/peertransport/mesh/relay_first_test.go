package mesh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// Lane H, support_25a0e3be: two machines behind one NAT whose signed endpoints were unreachable from each other
// (docker and LAN addresses, a public pair needing hairpin) never fell back to the relay: every attempt idled out.
// A relay must carry the traffic while no direct path is proven, as in Tailscale (DERP first, direct only after a
// disco pong). Two engines whose only signed candidates are unreachable exchange data over the relay at once.
func TestUnreachableSignedEndpointsStillUseTheRelay(t *testing.T) {
	// No direct path at all, as for the same-NAT pair: UDP is off, so only the relay can carry the traffic.
	envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	t.Cleanup(func() { envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "") })
	dm := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	region := dm.Regions[1]
	dialerKey, hostKey := key.NewNode(), key.NewNode()
	dialerAddr, hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::50"), netip.MustParseAddr("fd7a:115c:a1e0::51")
	host := &Server{Key: hostKey, LocalAddr: hostAddr, Region: region, AllowedPeers: map[key.NodePublic]netip.Addr{dialerKey.Public(): dialerAddr}, ServedUDPPorts: []filter.PortRange{{First: 443, Last: 443}}, OnUDP: func(uint16) func(ConnPacketConn) {
		return func(connection ConnPacketConn) {
			buffer := make([]byte, 64)
			for {
				n, err := connection.Read(buffer)
				if err != nil {
					return
				}
				_, _ = connection.Write(buffer[:n])
			}
		}
	}}
	dialer := &Server{Key: dialerKey, LocalAddr: dialerAddr, Region: region, AllowedPeers: map[key.NodePublic]netip.Addr{hostKey.Public(): hostAddr}, ServedUDPPorts: []filter.PortRange{}}
	for _, server := range []*Server{host, dialer} {
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
	}
	// TEST-NET-1 (RFC 5737): nothing answers there, like a peer's docker or LAN address seen from another network.
	unreachable := []netip.AddrPort{netip.MustParseAddrPort("192.0.2.10:41641"), netip.MustParseAddrPort("192.0.2.11:41641")}
	if err := host.SetPeerContacts(map[key.NodePublic]PeerContact{dialerKey.Public(): {Disco: dialer.lb.discoPublic(), Endpoints: unreachable}}); err != nil {
		t.Fatal(err)
	}
	if err := dialer.SetPeerContacts(map[key.NodePublic]PeerContact{hostKey.Public(): {Disco: host.lb.discoPublic(), Endpoints: unreachable}}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	connection, err := dialer.DialAuthorizedUDP(ctx, hostKey.Public(), host.lb.discoPublic(), netip.AddrPortFrom(hostAddr, 443))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(started.Add(6 * time.Second))
	if _, err := connection.Write([]byte("relay")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 5)
	if n, err := connection.Read(buffer); err != nil || string(buffer[:n]) != "relay" {
		t.Fatalf("no data within 6 s although a relay is available: %q err %v", buffer[:n], err)
	}
	t.Logf("first echo over the relay after %v (path %s)", time.Since(started), dialer.PeerPath(hostKey.Public()))
}
