package mesh

import (
	"context"
	"io"
	"net/netip"
	"slices"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

func TestPeerRestartWithSameIdentityReplacesItsPath(t *testing.T) {
	t.Run("direct", func(t *testing.T) { testRestartedPeerPath(t, nil) })
	t.Run("relay", func(t *testing.T) {
		envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
		t.Cleanup(func() { envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "") })
		regions := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
		testRestartedPeerPath(t, regions.Regions[1])
	})
}

func testRestartedPeerPath(t *testing.T, region *tailcfg.DERPRegion) {
	clientKey, hostKey := key.NewNode(), key.NewNode()
	clientAddr := netip.MustParseAddr("fd7a:115c:a1e0::30")
	hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::31")
	relayContacts := make(map[*Server][]netip.AddrPort)
	start := func(k key.NodePrivate, addr netip.Addr, peer key.NodePublic, peerAddr netip.Addr) *Server {
		t.Helper()
		s := &Server{Key: k, LocalAddr: addr, Region: region, AllowedPeers: map[key.NodePublic]netip.Addr{peer: peerAddr},
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
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if region != nil {
			// UDP is disabled: model changed signed candidates while real traffic uses DERP.
			relayContacts[s] = []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("192.0.2.10"), uint16(40000+len(relayContacts)))}
		}
		return s
	}
	endpoints := func(s *Server) []netip.AddrPort {
		if region != nil {
			return relayContacts[s]
		}
		return restartPeerEndpoints(t, s)
	}
	client := start(clientKey, clientAddr, hostKey.Public(), hostAddr)
	host := start(hostKey, hostAddr, clientKey.Public(), clientAddr)
	contacts := func(to, from *Server) {
		t.Helper()
		contact := PeerContact{Disco: from.lb.discoPublic(), Endpoints: endpoints(from)}
		if err := to.SetPeerContacts(map[key.NodePublic]PeerContact{from.Key.Public(): contact}); err != nil {
			t.Fatal(err)
		}
	}
	contacts(client, host)
	contacts(host, client)
	restartExchange(t, client, host)
	restartExchange(t, host, client)
	previous := endpoints(host)
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.SetPeerContacts(nil); err != nil {
		t.Fatal(err)
	}
	host = start(hostKey, hostAddr, clientKey.Public(), clientAddr)
	if slices.Equal(previous, endpoints(host)) {
		t.Fatal("fixture reused the prior listener endpoints")
	}
	contacts(host, client)
	contacts(client, host)
	restartExchange(t, client, host)
	restartExchange(t, host, client)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client = start(clientKey, clientAddr, hostKey.Public(), hostAddr)
	contacts(client, host)
	contacts(host, client)
	restartExchange(t, host, client)
	restartExchange(t, client, host)
}

func restartExchange(t *testing.T, from, to *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	c, err := from.DialAuthorizedUDP(ctx, to.Key.Public(), to.lb.discoPublic(), netip.AddrPortFrom(to.Addr(), 443))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	_ = c.SetDeadline(deadline)
	if _, err := c.Write([]byte("restart")); err != nil {
		t.Fatal(err)
	}
	var body [7]byte
	if _, err := io.ReadFull(c, body[:]); err != nil || string(body[:]) != "restart" {
		t.Fatalf("same-identity carrier unreachable after listener replacement: %q, %v", body, err)
	}
}

func restartPeerEndpoints(t *testing.T, s *Server) []netip.AddrPort {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var endpoints []netip.AddrPort
		for _, ep := range s.LocalEndpoints() {
			if ep.Addr().IsLoopback() || ep.Addr().IsPrivate() {
				endpoints = append(endpoints, ep)
			}
		}
		if len(endpoints) != 0 {
			return endpoints[:min(len(endpoints), maxPeerContactEndpoints)]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fixture has no local listener endpoints")
	return nil
}
