package mesh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"
)

// A live outgoing UDP lease must survive the remote engine losing its keys.
// Keeping that lease also reproduces overlapping QUIC dials preventing retirement.
func TestRestartedPeerAnswersExistingFlowWithinOneSecond(t *testing.T) {
	echo := func(c ConnPacketConn) {
		defer c.Close()
		b := make([]byte, 64)
		for {
			n, e := c.Read(b)
			if e != nil {
				return
			}
			if _, e = c.Write(b[:n]); e != nil {
				return
			}
		}
	}
	client, old := startUDPLifecyclePair(t, time.Minute, echo)
	flow := dialUDPLifecycleFlow(t, client, old)
	exchange := func(bound time.Duration) {
		t.Helper()
		_ = flow.SetDeadline(time.Now().Add(bound))
		if _, e := flow.Write([]byte("echo")); e != nil {
			t.Fatal(e)
		}
		var b [4]byte
		if n, e := flow.Read(b[:]); e != nil || n != 4 || string(b[:n]) != "echo" {
			t.Fatalf("peer did not answer within %s: %v", bound, e)
		}
	}
	exchange(10 * time.Second)
	if e := old.Close(); e != nil {
		t.Fatal(e)
	}
	next := &Server{
		Key: old.Key, LocalAddr: old.LocalAddr,
		AllowedPeers: map[key.NodePublic]netip.Addr{client.Key.Public(): client.LocalAddr},
		Region:       old.Region,
		PeerContacts: map[key.NodePublic]PeerContact{client.Key.Public(): {
			Disco: client.lb.discoPublic(), Endpoints: client.LocalEndpoints(),
		}},
		ServedUDPPorts: []filter.PortRange{{First: 53, Last: 53}},
		OnUDP:          func(uint16) func(ConnPacketConn) { return echo },
	}
	if e := next.Start(); e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	readyAt := time.Now()
	// Rendezvous is repeated by new native dials without destroying existing leases.
	ctx, cancel := context.WithDeadline(t.Context(), readyAt.Add(time.Second))
	defer cancel()
	if e := client.prepareAuthorizedPeer(ctx, next.Key.Public(), next.lb.discoPublic()); e != nil {
		t.Fatal(e)
	}
	deadline := readyAt.Add(time.Second)
	for {
		_ = flow.SetDeadline(time.Now().Add(100 * time.Millisecond))
		if _, e := flow.Write([]byte("echo")); e != nil {
			t.Fatal(e)
		}
		var b [4]byte
		if n, e := flow.Read(b[:]); e == nil && n == 4 && string(b[:]) == "echo" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restarted peer unreachable one second after listener start")
		}
	}
}
