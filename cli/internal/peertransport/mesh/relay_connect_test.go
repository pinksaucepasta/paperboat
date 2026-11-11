package mesh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// deadRelay is a relay region whose server accepts and drops every connection, counting the connects.
func deadRelay(t *testing.T, id tailcfg.DERPRegionID) (*tailcfg.DERPRegion, *atomic.Int64, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var connects atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connects.Add(1)
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	name := "dead" + string(rune('0'+id))
	return &tailcfg.DERPRegion{RegionID: id, RegionCode: name, Nodes: []*tailcfg.DERPNode{{Name: name, RegionID: id, HostName: "127.0.0.1", IPv4: "127.0.0.1", DERPPort: port, STUNPort: -1, InsecureForTests: true}}}, &connects, ln
}

// relayConnectsWhileAllDown dials peers handshakes at once for window while both relays of the engine are down and
// returns how many connects the relays saw.
func relayConnectsWhileAllDown(t *testing.T, peers int, window time.Duration) int64 {
	t.Helper()
	one, oneConnects, oneListener := deadRelay(t, 1)
	two, twoConnects, twoListener := deadRelay(t, 2)
	allowed := map[key.NodePublic]netip.Addr{}
	contacts := map[key.NodePublic]PeerContact{}
	for i := range peers {
		k := key.NewNode().Public()
		allowed[k] = netip.AddrFrom16([16]byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0, 14: byte(i >> 8), 15: byte(i + 2)})
		contacts[k] = PeerContact{Disco: key.NewDisco().Public()}
	}
	s := &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::1"), AllowedPeers: allowed}
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{one, two}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { _ = s.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			// Pre-existing, also on the code before this test: wireguard-go's BindClose waits on a UDP receive
			// that never returns once the engine's relays went away. Reported separately; not this test's subject.
			t.Log("the engine did not close within 10 s after its relays went away (known, reported)")
		}
	})
	if err := s.SetPeerContacts(contacts); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()
	var dials sync.WaitGroup
	for k, c := range contacts {
		dials.Go(func() {
			if conn, err := s.DialAuthorizedUDP(ctx, k, c.Disco, netip.AddrPortFrom(allowed[k], 443)); err == nil {
				_ = conn.Close()
				t.Error("a handshake succeeded with every relay down")
			}
		})
	}
	dials.Wait()
	connects := oneConnects.Load() + twoConnects.Load()
	_, _ = oneListener.Close(), twoListener.Close()
	return connects
}

// With the account's whole relay pair down, unanswered handshakes to many peers must not multiply relay connects:
// one connect per region is in flight at a time for all peers, a failed region is held for relayConnectHold, and
// each peer probes the other relays on a backoff. 24 peers cost about what one peer costs.
func TestRelayConnectsStayBoundedWithEveryRelayDown(t *testing.T) {
	envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	t.Cleanup(func() { envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "") })
	const window = 7 * time.Second
	single := relayConnectsWhileAllDown(t, 1, window)
	many := relayConnectsWhileAllDown(t, 24, window)
	t.Logf("relay connects in %v with every relay down: 1 peer %d, 24 peers %d", window, single, many)
	// Without the shared limit, 24 peers made 4.5 times one peer's connects (278 vs 62 in 7 s). With it, the connects
	// are magicsock's own home reconnects plus about one per region per hold, whatever the number of peers.
	if limit := 2 * single; many > limit {
		t.Fatalf("24 peers made %d relay connects, 1 peer %d: want at most %d", many, single, limit)
	}
}

// Only the first answer to our outstanding handshake moves the peer, and only one that echoes its nonce: a late answer
// to an earlier handshake, a bare answer (a peer without nonces) and a second answer from another relay do not.
func TestOnlyTheAnswerToOurHandshakeRecordsItsRegion(t *testing.T) {
	regions := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	one, two := regions.Regions[1], relayRegionCopy(regions.Regions[1], 2)
	peer := key.NewNode().Public()
	s := &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::50"),
		AllowedPeers: map[key.NodePublic]netip.Addr{peer: netip.MustParseAddr("fd7a:115c:a1e0::51")}}
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{one, two}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public()}}); err != nil {
		t.Fatal(err)
	}
	outstanding := func(nonce byte) *meowWait {
		w := &meowWait{ready: make(chan struct{}), nonce: [meowNonceLen]byte{nonce}}
		s.peerReadyMu.Lock()
		if s.peerReady == nil {
			s.peerReady = map[key.NodePublic]*meowWait{}
		}
		s.peerReady[peer] = w
		s.peerReadyMu.Unlock()
		return w
	}
	answered := func(w *meowWait) bool {
		select {
		case <-w.ready:
			return true
		default:
			return false
		}
	}
	answer := func(region tailcfg.DERPRegionID, pkt []byte) {
		nonce, echoed := meowedNonce(pkt)
		s.meowed(peer, region, nonce, echoed)
	}
	if got := s.lb.peerDERPRegion(peer); got != one.RegionID {
		t.Fatalf("fixture: peer starts on %d, want %d", got, one.RegionID)
	}
	w := outstanding(7)
	answer(two.RegionID, EncodeMeowed([meowNonceLen]byte{6})) // late answer to an earlier handshake
	if answered(w) || s.lb.peerDERPRegion(peer) != one.RegionID {
		t.Fatal("an answer to another handshake was taken")
	}
	answer(two.RegionID, EncodeMeowed(w.nonce))
	if !answered(w) || s.lb.peerDERPRegion(peer) != two.RegionID {
		t.Fatalf("the answer to our handshake: answered %v, region %d", answered(w), s.lb.peerDERPRegion(peer))
	}
	answer(one.RegionID, EncodeMeowed(w.nonce)) // the same answer again, from the peer's other relay
	if s.lb.peerDERPRegion(peer) != two.RegionID {
		t.Fatal("a second answer to an answered handshake moved the peer")
	}
	bare := outstanding(9)
	answer(one.RegionID, EncodeMeowed([meowNonceLen]byte{}))
	if !answered(bare) || s.lb.peerDERPRegion(peer) != two.RegionID {
		t.Fatal("a bare answer must complete the handshake without moving the peer")
	}
}

// Review of b6eb2018c, P1: an answer to our handshake racing a contact change (a peer that moved relay) deadlocked the
// engine. meowed held peerReadyMu while setPeerRelayRegion took policyMu; SetPeerContacts and InvalidateAuthorizedPeer
// hold policyMu while they take peerReadyMu. The test forces that interleaving: it holds policyMu, lets the answer
// arrive, then invalidates the peer under policyMu, which must not wait on the answer.
func TestHandshakeAnswerRacingAContactChangeDoesNotDeadlock(t *testing.T) {
	regions := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	one, two := regions.Regions[1], relayRegionCopy(regions.Regions[1], 2)
	peer := key.NewNode().Public()
	s := &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::70"),
		AllowedPeers: map[key.NodePublic]netip.Addr{peer: netip.MustParseAddr("fd7a:115c:a1e0::71")}}
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{one, two}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public()}}); err != nil {
		t.Fatal(err)
	}
	w := &meowWait{ready: make(chan struct{}), nonce: [meowNonceLen]byte{5}}
	s.peerReadyMu.Lock()
	s.peerReady = map[key.NodePublic]*meowWait{peer: w}
	s.peerReadyMu.Unlock()

	s.lb.policyMu.Lock() // a contact change is under way
	answered := make(chan struct{})
	go func() { s.meowed(peer, two.RegionID, w.nonce, true); close(answered) }()
	time.Sleep(100 * time.Millisecond) // the answer is now waiting for policyMu inside setPeerRelayRegion
	invalidated := make(chan struct{})
	go func() { s.invalidateAuthorizedPeerLocked(peer); close(invalidated) }()
	select {
	case <-invalidated:
	case <-time.After(3 * time.Second):
		s.lb.policyMu.Unlock() // unwind the deadlock so the test binary can finish
		<-invalidated
		t.Fatal("invalidating a peer under policyMu waited on a handshake answer: ABBA deadlock")
	}
	s.lb.policyMu.Unlock()
	select {
	case <-answered:
	case <-time.After(3 * time.Second):
		t.Fatal("the handshake answer did not finish after the contact change")
	}
}

// Review of b6eb2018c, P4: a failed nonce source was ignored and the handshake went out with a zero nonce. It now
// fails the handshake before anything is sent, and leaves no waiter behind.
func TestHandshakeFailsWhenTheNonceSourceFails(t *testing.T) {
	regions := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	peer := key.NewNode().Public()
	s := &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::80"), Region: regions.Regions[1],
		AllowedPeers: map[key.NodePublic]netip.Addr{peer: netip.MustParseAddr("fd7a:115c:a1e0::81")}}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	failed := errors.New("entropy unavailable")
	old := meowNonceRead
	meowNonceRead = func([]byte) (int, error) { return 0, failed }
	t.Cleanup(func() { meowNonceRead = old })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := s.awaitMeow(ctx, peer); !errors.Is(err, failed) {
		t.Fatalf("handshake with a failed nonce source: %v", err)
	}
	s.peerReadyMu.Lock()
	_, left := s.peerReady[peer]
	s.peerReadyMu.Unlock()
	if left {
		t.Fatal("a failed handshake left a waiter with a zero nonce")
	}
}
