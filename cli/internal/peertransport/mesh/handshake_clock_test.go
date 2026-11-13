package mesh

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/tailscale/wireguard-go/tai64n"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"
)

func TestHandshakeIsStampedOnTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	handshakeStamp.last = time.Time{}
	serverclock.Observe(time.Now().Add(-10*time.Minute).UTC().Format(http.TimeFormat), time.Now(), time.Now())
	if got := time.Until(handshakeTime()); got > -9*time.Minute || got < -11*time.Minute {
		t.Fatalf("stamp %s from now with a local clock 10 min ahead, want about -10m", got)
	}
}

// X-27: a peer that accepted an initiation stamped 10 min ahead refuses its sender after the clock is set
// back; the raw clock reproduces that, the handshake clock does not.
func TestHandshakeAfterClockSetBack(t *testing.T) {
	var skew atomic.Int64
	raw := func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	for name, tc := range map[string]struct {
		clock func() time.Time
		works bool
	}{"raw clock": {raw, false}, "handshake clock": {handshakeTime, true}} {
		t.Run(name, func(t *testing.T) {
			skew.Store(int64(10 * time.Minute))
			handshakeBase = raw
			handshakeStamp.last = time.Time{}
			tai64n.Clock = tc.clock
			t.Cleanup(func() {
				handshakeBase = serverclock.Now
				tai64n.Clock = handshakeTime
				handshakeStamp.last = time.Time{}
			})
			clientKey, hostKey := key.NewNode(), key.NewNode()
			clientAddr, hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::40"), netip.MustParseAddr("fd7a:115c:a1e0::41")
			host := &Server{Key: hostKey, LocalAddr: hostAddr, AllowedPeers: map[key.NodePublic]netip.Addr{clientKey.Public(): clientAddr},
				ServedUDPPorts: []filter.PortRange{{First: 443, Last: 443}}, OnUDP: func(uint16) func(ConnPacketConn) {
					return func(c ConnPacketConn) {
						defer c.Close()
						var b [64]byte
						n, err := c.Read(b[:])
						if err == nil {
							_, _ = c.Write(b[:n])
						}
					}
				}}
			if err := host.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = host.Close() })
			client := &Server{Key: clientKey, LocalAddr: clientAddr, AllowedPeers: map[key.NodePublic]netip.Addr{hostKey.Public(): hostAddr}}
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			for _, pair := range [][2]*Server{{client, host}, {host, client}} {
				to, from := pair[0], pair[1]
				if err := to.SetPeerContacts(map[key.NodePublic]PeerContact{from.Key.Public(): {Disco: from.lb.discoPublic(), Endpoints: restartPeerEndpoints(t, from)}}); err != nil {
					t.Fatal(err)
				}
			}
			// The client drops its session and handshakes again; the host keeps its state for the client.
			dial := func(first bool) error {
				if !first {
					client.InvalidateAuthorizedPeer(hostKey.Public())
				}
				ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
				defer cancel()
				c, err := client.DialAuthorizedUDP(ctx, hostKey.Public(), host.lb.discoPublic(), netip.AddrPortFrom(host.Addr(), 443))
				if err != nil {
					return err
				}
				defer c.Close()
				deadline, _ := ctx.Deadline()
				_ = c.SetDeadline(deadline)
				if _, err := c.Write([]byte("clock")); err != nil {
					return err
				}
				var body [5]byte
				_, err = io.ReadFull(c, body[:])
				return err
			}
			if err := dial(true); err != nil {
				t.Fatalf("first handshake with the clock 10 min ahead: %v", err)
			}
			skew.Store(0)
			if err := dial(false); (err == nil) != tc.works {
				t.Fatalf("handshake after the clock was set back: err=%v, works=%v", err, tc.works)
			}
		})
	}
}
