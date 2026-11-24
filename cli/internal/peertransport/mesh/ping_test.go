package mesh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
	"tailscale.com/wgengine/filter"
)

const pingTestDelay = 60 * time.Millisecond

// slowPackets delays every packet it sends by pingTestDelay.
type slowPackets struct{ next nettype.PacketListener }

func (l slowPackets) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	conn, err := l.next.ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &slowConn{blackholeConn: conn.(*blackholeConn)}, nil
}

type slowConn struct{ *blackholeConn }

func (c *slowConn) WriteTo(p []byte, to net.Addr) (int, error) {
	body := append([]byte(nil), p...)
	time.AfterFunc(pingTestDelay, func() { _, _ = c.blackholeConn.WriteTo(body, to) })
	return len(p), nil
}

func (c *slowConn) WriteToUDPAddrPort(p []byte, to netip.AddrPort) (int, error) {
	body := append([]byte(nil), p...)
	time.AfterFunc(pingTestDelay, func() { _, _ = c.blackholeConn.WriteToUDPAddrPort(body, to) })
	return len(p), nil
}

// Every sample is a round trip to the peer: the second, on the held connection, shows the link's delay too, and a
// peer that stops answering is a lost sample although the connection is still held.
func TestPingIsARoundTripNotTheHeldConnection(t *testing.T) {
	relay := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	var drop atomic.Bool
	dialerKey, hostKey := key.NewNode(), key.NewNode()
	dialerAddr, hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::80"), netip.MustParseAddr("fd7a:115c:a1e0::81")
	start := func(k key.NodePrivate, addr netip.Addr, peer key.NodePublic, peerAddr netip.Addr) *Server {
		s := &Server{Key: k, LocalAddr: addr, AllowedPeers: map[key.NodePublic]netip.Addr{peer: peerAddr}, TestOnlyPacketListener: slowPackets{blackholePackets{&drop}}}
		s.ServedUDPPorts = []filter.PortRange{{First: 443, Last: 443}}
		s.OnUDP = func(uint16) func(ConnPacketConn) {
			return func(c ConnPacketConn) {
				buffer := make([]byte, 2048)
				for {
					n, err := c.Read(buffer)
					if err != nil {
						return
					}
					_, _ = c.Write(buffer[:n])
				}
			}
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
	host := start(hostKey, hostAddr, dialerKey.Public(), dialerAddr)
	dialer := start(dialerKey, dialerAddr, hostKey.Public(), hostAddr)
	if err := host.SetPeerContacts(map[key.NodePublic]PeerContact{dialerKey.Public(): {Disco: dialer.lb.discoPublic(), Endpoints: restartPeerEndpoints(t, dialer)}}); err != nil {
		t.Fatal(err)
	}
	if err := dialer.SetPeerContacts(map[key.NodePublic]PeerContact{hostKey.Public(): {Disco: host.lb.discoPublic(), Endpoints: restartPeerEndpoints(t, host)}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	held, err := dialer.DialAuthorizedUDP(ctx, hostKey.Public(), host.lb.discoPublic(), netip.AddrPortFrom(hostAddr, 443))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for dialer.PeerPath(hostKey.Public()) != "direct" || host.PeerPath(dialerKey.Public()) != "direct" {
		if ctx.Err() != nil {
			t.Fatalf("no direct path (%q, %q)", dialer.PeerPath(hostKey.Public()), host.PeerPath(dialerKey.Public()))
		}
		if _, err := dialer.Ping(ctx, hostKey.Public()); err != nil {
			t.Fatal(err)
		}
		_, _ = host.Ping(ctx, dialerKey.Public())
		time.Sleep(50 * time.Millisecond)
	}
	for sample := range 2 {
		rtt, err := dialer.Ping(ctx, hostKey.Public())
		if err != nil || rtt < 2*pingTestDelay || rtt > 2*time.Second {
			t.Fatalf("sample %d: rtt %v, %v; want at least %v", sample+1, rtt, err, 2*pingTestDelay)
		}
	}
	_ = host.Close()
	drop.Store(true)
	lost, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if rtt, err := dialer.Ping(lost, hostKey.Public()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a peer that stopped answering gave rtt %v, %v", rtt, err)
	}
}
