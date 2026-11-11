package mesh

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/net/tstun"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// blackholePackets is a UDP listener whose sockets silently discard everything in both directions once drop is
// set: a direct path that dies without any error, as a NAT mapping or a Wi-Fi roam does. Relay traffic is TCP and
// is not affected.
type blackholePackets struct{ drop *atomic.Bool }

func (b blackholePackets) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	var lc net.ListenConfig
	conn, err := lc.ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &blackholeConn{UDPConn: conn.(*net.UDPConn), drop: b.drop}, nil
}

type blackholeConn struct {
	*net.UDPConn
	drop *atomic.Bool
}

func (c *blackholeConn) WriteTo(p []byte, to net.Addr) (int, error) {
	if c.drop.Load() {
		return len(p), nil
	}
	return c.UDPConn.WriteTo(p, to)
}

func (c *blackholeConn) WriteToUDPAddrPort(p []byte, to netip.AddrPort) (int, error) {
	if c.drop.Load() {
		return len(p), nil
	}
	return c.UDPConn.WriteToUDPAddrPort(p, to)
}

func (c *blackholeConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, from, err := c.UDPConn.ReadFrom(p)
		if err != nil || !c.drop.Load() {
			return n, from, err
		}
	}
}

func (c *blackholeConn) ReadFromUDPAddrPort(p []byte) (int, netip.AddrPort, error) {
	for {
		n, from, err := c.UDPConn.ReadFromUDPAddrPort(p)
		if err != nil || !c.drop.Load() {
			return n, from, err
		}
	}
}

// F21-160 (PLAN 3b gap 13): a direct path that dies silently is noticed in seconds, not by a two minute idle
// timeout. Two engines with a relay and each other's endpoints connect directly; the direct UDP path is then
// blackholed and an echo stream must come back over the relay within the bound (Tailscale's heartbeat 3 s and
// trustUDPAddrDuration 6.5 s, ~/ref/tailscale wgengine/magicsock/magicsock.go:4084-4088).
func TestDeadDirectPathFallsBackToTheRelayInSeconds(t *testing.T) {
	relay := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	var drop atomic.Bool
	dialerKey, hostKey := key.NewNode(), key.NewNode()
	dialerAddr, hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::70"), netip.MustParseAddr("fd7a:115c:a1e0::71")
	echo := func(uint16) func(ConnPacketConn) {
		return func(connection ConnPacketConn) {
			buffer := make([]byte, 2048)
			for {
				n, err := connection.Read(buffer)
				if err != nil {
					return
				}
				_, _ = connection.Write(buffer[:n])
			}
		}
	}
	start := func(k key.NodePrivate, addr netip.Addr, peer key.NodePublic, peerAddr netip.Addr, serve bool) *Server {
		s := &Server{Key: k, LocalAddr: addr, AllowedPeers: map[key.NodePublic]netip.Addr{peer: peerAddr}, TestOnlyPacketListener: blackholePackets{&drop}}
		if serve {
			s.ServedUDPPorts, s.OnUDP = []filter.PortRange{{First: 443, Last: 443}}, echo
		}
		if err := s.SetRelayRegions([]*tailcfg.DERPRegion{relay}); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := s.PrepareRelay(ctx, relay.RegionID); err != nil {
			t.Fatal(err)
		}
		return s
	}
	host := start(hostKey, hostAddr, dialerKey.Public(), dialerAddr, true)
	dialer := start(dialerKey, dialerAddr, hostKey.Public(), hostAddr, false)
	endpoints := func(s *Server) []netip.AddrPort {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var local []netip.AddrPort
			for _, ep := range s.LocalEndpoints() {
				if ep.Addr().IsLoopback() || ep.Addr().IsPrivate() {
					local = append(local, ep)
				}
			}
			if len(local) > 0 {
				return local[:min(len(local), maxPeerContactEndpoints)]
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("the engine found no local endpoint")
		return nil
	}
	if err := host.SetPeerContacts(map[key.NodePublic]PeerContact{dialerKey.Public(): {Disco: dialer.lb.discoPublic(), Endpoints: endpoints(dialer)}}); err != nil {
		t.Fatal(err)
	}
	if err := dialer.SetPeerContacts(map[key.NodePublic]PeerContact{hostKey.Public(): {Disco: host.lb.discoPublic(), Endpoints: endpoints(host)}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	connection, err := dialer.DialAuthorizedUDP(ctx, hostKey.Public(), host.lb.discoPublic(), netip.AddrPortFrom(hostAddr, 443))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	fullSize := bytes.Repeat([]byte{0xa5}, MaxUDPPayload)
	ping := func(within time.Duration) bool {
		_ = connection.SetDeadline(time.Now().Add(within))
		// A full-size datagram: neither path may drop what the tunnel MTU allows.
		if _, err := connection.Write(fullSize); err != nil {
			return false
		}
		buffer := make([]byte, 2048)
		n, err := connection.Read(buffer)
		return err == nil && bytes.Equal(buffer[:n], fullSize)
	}
	deadline := time.Now().Add(15 * time.Second)
	for dialer.PeerPath(hostKey.Public()) != "direct" || !ping(time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("no direct path to start from (path %q)", dialer.PeerPath(hostKey.Public()))
		}
		time.Sleep(100 * time.Millisecond)
	}
	drop.Store(true)
	cut := time.Now()
	for !ping(500 * time.Millisecond) {
		if took := time.Since(cut); took > deadlinePathRecovery {
			t.Fatalf("no echo %v after the direct path died (path %q), want within %v", took, dialer.PeerPath(hostKey.Public()), deadlinePathRecovery)
		}
	}
	t.Logf("echo back %v after the direct path died; path %q", time.Since(cut), dialer.PeerPath(hostKey.Public()))
}

// deadlinePathRecovery is the row's "seconds": 6.5 s measured (trustUDPAddrDuration), with headroom for a loaded runner.
const deadlinePathRecovery = 12 * time.Second

// F21-160: large packets are not silently dropped because the tunnel MTU is fixed at the safe 1280 (the IPv6
// minimum) with MaxUDPPayload fitting it exactly, as Tailscale's default; its peer path MTU discovery is opt-in
// (~/ref/tailscale net/tstun/mtu.go safeTUNMTU, wgengine/magicsock/peermtu.go ShouldPMTUD).
func TestUDPPayloadFitsTheSafeTunnelMTU(t *testing.T) {
	if got := MaxUDPPayload + 40 + 8; got != int(tstun.DefaultTUNMTU()) {
		t.Fatalf("MaxUDPPayload %d + IPv6 and UDP headers = %d, tunnel MTU is %d", MaxUDPPayload, got, tstun.DefaultTUNMTU())
	}
}
