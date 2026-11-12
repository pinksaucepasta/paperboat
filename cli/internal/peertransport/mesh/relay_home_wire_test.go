package mesh

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// countedRegion fronts the relay of region with a TCP proxy and counts the connections the engines open to it: an
// engine connects to a region only when it homes there or sends there, so the count is what the relay sees on the wire.
func countedRegion(t *testing.T, region *tailcfg.DERPRegion) (*tailcfg.DERPRegion, *atomic.Int64) {
	t.Helper()
	counted := new(atomic.Int64)
	front := *region
	front.Nodes = nil
	for _, node := range region.Nodes {
		backend := net.JoinHostPort(node.IPv4, strconv.Itoa(node.DERPPort))
		ln, err := net.Listen("tcp", net.JoinHostPort(node.IPv4, "0"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				in, err := ln.Accept()
				if err != nil {
					return
				}
				counted.Add(1)
				out, err := net.Dial("tcp", backend)
				if err != nil {
					_ = in.Close()
					continue
				}
				go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
				go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
			}
		}()
		fronted := *node
		fronted.DERPPort = ln.Addr().(*net.TCPAddr).Port
		front.Nodes = append(front.Nodes, &fronted)
	}
	return &front, counted
}

// F21-161 wire effect: two machines whose selected relay is region 2 meet there, and the relay of region 1 (the lower
// id, the one an alphabetical choice would use) sees no connection at all; with region 1 selected it is the other way.
func TestPeersMeetOnTheSelectedRelayAndNotTheLowestID(t *testing.T) {
	envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	t.Cleanup(func() { envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "") })
	for _, selected := range []tailcfg.DERPRegionID{2, 1} {
		t.Run("selected region "+strconv.Itoa(int(selected)), func(t *testing.T) {
			one, oneCount := countedRegion(t, integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1])
			twoRegion, twoCount := countedRegion(t, relayRegionCopy(integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1], 2))
			regions := []*tailcfg.DERPRegion{one, twoRegion}
			if selected == 2 {
				regions = []*tailcfg.DERPRegion{twoRegion, one}
			}
			clientKey, hostKey := key.NewNode(), key.NewNode()
			clientAddr, hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::70"), netip.MustParseAddr("fd7a:115c:a1e0::71")
			start := func(k key.NodePrivate, addr netip.Addr, peer key.NodePublic, peerAddr netip.Addr) *Server {
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
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if err := s.PrepareRelay(ctx, regions[0].RegionID); err != nil {
					t.Fatal(err)
				}
				return s
			}
			client := start(clientKey, clientAddr, hostKey.Public(), hostAddr)
			host := start(hostKey, hostAddr, clientKey.Public(), clientAddr)
			contact := func(to, from *Server) {
				c := PeerContact{Disco: from.lb.discoPublic(), Endpoints: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("192.0.2.20"), 40001)}}
				if err := to.SetPeerContacts(map[key.NodePublic]PeerContact{from.Key.Public(): c}); err != nil {
					t.Fatal(err)
				}
			}
			contact(client, host)
			contact(host, client)
			restartExchange(t, client, host)
			chosen, other := twoCount, oneCount
			if selected == 1 {
				chosen, other = oneCount, twoCount
			}
			if chosen.Load() < 2 || other.Load() != 0 {
				t.Fatalf("selected region %d: relay connections selected=%d other=%d, want both machines on the selected relay and none on the other", selected, chosen.Load(), other.Load())
			}
		})
	}
}
