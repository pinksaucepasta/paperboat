package mesh

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"sync"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// hop-89: a Windows machine restarted after an update and homed on the other relay of the account's pair. Linux test machine kept
// sending its handshake to the relay it remembered, which the restarted machine was no longer on, so every exec
// failed until something else moved Linux test machine. The peer's signed home now replaces the remembered region (one try), and
// without one the answer to a probe on the other relays is recorded (a bounded fallback).
func TestPeerRestartedOnTheOtherRelayIsReached(t *testing.T) {
	envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	t.Cleanup(func() { envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "") })
	one := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	two := relayRegionCopy(integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1], 2)
	t.Run("signed home", func(t *testing.T) { testPeerMovedRelay(t, one, two, true, time.Second) })
	t.Run("answer from a probe", func(t *testing.T) { testPeerMovedRelay(t, one, two, false, 4*time.Second) })
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

func testPeerMovedRelay(t *testing.T, one, two *tailcfg.DERPRegion, signed bool, within time.Duration) {
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
	contact := func(to, from *Server, home tailcfg.DERPRegionID) {
		t.Helper()
		c := PeerContact{Disco: from.lb.discoPublic(), Home: home,
			Endpoints: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("192.0.2.20"), uint16(40000+started))}}
		if err := to.SetPeerContacts(map[key.NodePublic]PeerContact{from.Key.Public(): c}); err != nil {
			t.Fatal(err)
		}
	}
	client := start(clientKey, clientAddr, hostKey.Public(), hostAddr, one, two)
	host := start(hostKey, hostAddr, clientKey.Public(), clientAddr, one)
	var home tailcfg.DERPRegionID
	if signed {
		home = one.RegionID
	}
	contact(client, host, home)
	contact(host, client, 0)
	restartExchange(t, host, client) // the client learns the host's relay from its handshake
	restartExchange(t, client, host)
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	host = start(hostKey, hostAddr, clientKey.Public(), clientAddr, two) // same identity, now on the other relay only
	contact(host, client, 0)
	if signed {
		home = two.RegionID
	}
	contact(client, host, home)
	begin := time.Now()
	restartExchange(t, client, host)
	if took := time.Since(begin); took > within {
		t.Fatalf("reached the moved peer after %v, want within %v", took, within)
	}
}

// F21-161 (PLAN 3b gap 14): the home region a machine advertises to its peers is the first region of the
// inventory it was given (the selected relay), not the lowest region id, and promoting the other relay
// moves it. Real relay regions, a started engine, the self node the peers are told about.
func TestAdvertisedHomeIsTheSelectedRegionNotTheLowestID(t *testing.T) {
	one := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	two := relayRegionCopy(integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1], 2)
	local := key.NewNode()
	s := &Server{Key: local, LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::50"), AllowedPeers: map[key.NodePublic]netip.Addr{key.NewNode().Public(): netip.MustParseAddr("fd7a:115c:a1e0::51")}}
	// Region 2 is selected although region 1 has the lower id.
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{two, one}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	advertised := func() tailcfg.DERPRegionID {
		s.lb.mu.Lock()
		defer s.lb.mu.Unlock()
		return s.lb.nm.SelfNode.HomeDERP()
	}
	if got := advertised(); got != 2 {
		t.Fatalf("advertised home = %d, want the selected region 2", got)
	}
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{one, two}); err != nil {
		t.Fatal(err)
	}
	if got := advertised(); got != 1 {
		t.Fatalf("after promoting region 1 the advertised home = %d", got)
	}
}

// F21-29: the only STUN servers the engine can ask are the relays it was given. magicsock and netcheck probe the
// DERP map's nodes (STUNPort), and the map the engine installs is exactly the relay inventory: no other region,
// no extra node, and no other address in a node.
func TestTheDERPMapHoldsOnlyTheGivenRelays(t *testing.T) {
	one := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	two := relayRegionCopy(integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1], 2)
	s := &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::60"), AllowedPeers: map[key.NodePublic]netip.Addr{key.NewNode().Public(): netip.MustParseAddr("fd7a:115c:a1e0::61")}}
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{one, two}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	given := map[string]bool{}
	for _, region := range []*tailcfg.DERPRegion{one, two} {
		for _, node := range region.Nodes {
			given[fmt.Sprint(region.RegionID, node.HostName, node.STUNPort, node.IPv4, node.IPv6, node.STUNTestIP, node.STUNOnly)] = true
		}
	}
	s.lb.mu.Lock()
	defer s.lb.mu.Unlock()
	if len(s.lb.dm.Regions) != 2 {
		t.Fatalf("DERP map has %d regions, want the 2 relays", len(s.lb.dm.Regions))
	}
	for id, region := range s.lb.dm.Regions {
		for _, node := range region.Nodes {
			if !given[fmt.Sprint(id, node.HostName, node.STUNPort, node.IPv4, node.IPv6, node.STUNTestIP, node.STUNOnly)] {
				t.Fatalf("region %d node %+v is not one of the given relays", id, node)
			}
		}
	}
}

// startRoutesServer starts an engine homed on region two with regions one and three also in its map, and one admitted
// peer per name; routeTTL is how long its reply routes are fresh.
func startRoutesServer(t *testing.T, routeTTL time.Duration, peers ...key.NodePublic) (s *Server, one, two, three *tailcfg.DERPRegion) {
	t.Helper()
	regions := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	one, two, three = regions.Regions[1], relayRegionCopy(regions.Regions[1], 2), relayRegionCopy(regions.Regions[1], 3)
	allowed := map[key.NodePublic]netip.Addr{}
	for i, p := range peers {
		allowed[p] = netip.AddrFrom16([16]byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0, 15: byte(i + 1)})
	}
	s = &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::ff"), AllowedPeers: allowed}
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{two, one, three}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.lb.routeTTL = routeTTL
	return s, one, two, three
}

// peerHome is the signed home the engine stores for peer.
func peerHome(s *Server, peer key.NodePublic) tailcfg.DERPRegionID {
	s.lb.mu.Lock()
	defer s.lb.mu.Unlock()
	return s.lb.contacts[peer].Home
}

// nodeRegion is the HomeDERP of peer's netmap node, the region magicsock sends by.
func nodeRegion(s *Server, peer key.NodePublic) tailcfg.DERPRegionID {
	s.lb.mu.Lock()
	defer s.lb.mu.Unlock()
	if n := s.lb.clients[peer]; n != nil {
		return n.HomeDERP
	}
	return 0
}

func awaitRegion(t *testing.T, s *Server, peer key.NodePublic, want tailcfg.DERPRegionID, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got, node := s.lb.peerDERPRegion(peer), nodeRegion(s, peer); got == want && node == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("peer is on region %d (netmap node %d), want %d", got, node, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An answer or a ping that arrives on another relay is a reply route: the peer is reached there while it is fresh, and
// its signed home is never rewritten (magicsock keeps derpRoute apart from derpAddr). A handshake answered on region two
// while the peer's home is one does not move the home.
func TestAnsweredRegionIsAReplyRouteNotTheHome(t *testing.T) {
	peer := key.NewNode().Public()
	s, one, two, _ := startRoutesServer(t, time.Hour, peer)
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public(), Home: one.RegionID}}); err != nil {
		t.Fatal(err)
	}
	awaitRegion(t, s, peer, one.RegionID, time.Second)
	w := &meowWait{ready: make(chan struct{}), nonce: [meowNonceLen]byte{3}}
	s.peerReadyMu.Lock()
	s.peerReady = map[key.NodePublic]*meowWait{peer: w}
	s.peerReadyMu.Unlock()
	s.meowed(peer, two.RegionID, w.nonce, testStart)
	select {
	case <-w.ready:
	default:
		t.Fatal("the answer did not complete the handshake")
	}
	awaitRegion(t, s, peer, two.RegionID, time.Second)
	if got := peerHome(s, peer); got != one.RegionID {
		t.Fatalf("the answer rewrote the stored home: %d, want %d", got, one.RegionID)
	}
}

// A ping reply (setPeerRelayRegion, as onDERPRecv calls it) never changes the stored home, and a renewal repeating the
// signed contact neither undoes the route nor moves the home.
func TestPingReplyNeverChangesTheStoredHome(t *testing.T) {
	peer := key.NewNode().Public()
	s, one, two, _ := startRoutesServer(t, time.Hour, peer)
	contact := PeerContact{Disco: key.NewDisco().Public(), Home: one.RegionID}
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: contact}); err != nil {
		t.Fatal(err)
	}
	if err := s.lb.setPeerRelayRegion(peer, two.RegionID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: contact}); err != nil { // the unchanged renewal
			t.Fatal(err)
		}
	}
	if got := peerHome(s, peer); got != one.RegionID {
		t.Fatalf("a ping reply changed the stored home to %d", got)
	}
	awaitRegion(t, s, peer, two.RegionID, time.Second)
}

// A reply route expires back to the signed home, in the netmap magicsock sends by as well as in the handshake.
func TestReplyRouteExpiresBackToTheHome(t *testing.T) {
	peer := key.NewNode().Public()
	s, one, two, _ := startRoutesServer(t, 300*time.Millisecond, peer)
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public(), Home: one.RegionID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerRelayRegion(peer, two.RegionID); err != nil {
		t.Fatal(err)
	}
	if got := s.lb.peerDERPRegion(peer); got != two.RegionID {
		t.Fatalf("fresh route not used: %d", got)
	}
	awaitRegion(t, s, peer, one.RegionID, 5*time.Second)
	// Another ping renews it.
	if err := s.SetPeerRelayRegion(peer, two.RegionID); err != nil {
		t.Fatal(err)
	}
	awaitRegion(t, s, peer, two.RegionID, time.Second)
}

// Without a signed home a route is the fallback however old (magicsock fallbackDERPRegionForPeer): the engine's own
// home is only a guess, so expiry must not trade the peer's last working relay for it.
func TestReplyRouteWithoutASignedHomeOutlivesItsTTL(t *testing.T) {
	peer := key.NewNode().Public()
	s, _, _, three := startRoutesServer(t, 100*time.Millisecond, peer)
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public()}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerRelayRegion(peer, three.RegionID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	awaitRegion(t, s, peer, three.RegionID, time.Second)
}

// A peer that restarts on another relay reports a new signed home; a route heard before it is older news.
func TestNewSignedHomeDropsTheOlderReplyRoute(t *testing.T) {
	peer := key.NewNode().Public()
	s, one, two, three := startRoutesServer(t, time.Hour, peer)
	disco := key.NewDisco().Public()
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: disco, Home: one.RegionID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerRelayRegion(peer, two.RegionID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: disco, Home: three.RegionID}}); err != nil {
		t.Fatal(err)
	}
	awaitRegion(t, s, peer, three.RegionID, time.Second)
}

// Recovery selections (Server.SetPeerRelayRegion) still work: they steer the peer, refuse a region this engine lacks or a
// peer it has not admitted, and survive the peer being re-added.
func TestRecoveryPinIsAReplyRoute(t *testing.T) {
	peer, stranger := key.NewNode().Public(), key.NewNode().Public()
	s, one, _, three := startRoutesServer(t, time.Hour, peer)
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public(), Home: one.RegionID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerRelayRegion(peer, three.RegionID); err != nil {
		t.Fatal(err)
	}
	awaitRegion(t, s, peer, three.RegionID, time.Second)
	if s.SetPeerRelayRegion(peer, 99) == nil || s.SetPeerRelayRegion(stranger, three.RegionID) == nil {
		t.Fatal("a region this engine lacks or an unadmitted peer was accepted")
	}
	if got := peerHome(s, peer); got != one.RegionID {
		t.Fatalf("a recovery pin changed the stored home to %d", got)
	}
	s.InvalidateAuthorizedPeer(peer) // a retired lease re-adds its peer on the rendezvous it had
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public(), Home: one.RegionID}}); err != nil {
		t.Fatal(err)
	}
	awaitRegion(t, s, peer, three.RegionID, time.Second)
	if err := s.ReplaceAllowedPeers(map[key.NodePublic]netip.Addr{}); err != nil { // a removed peer forgets it
		t.Fatal(err)
	}
	s.lb.mu.Lock()
	left := len(s.lb.replyRoutes)
	s.lb.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d reply routes outlived their peers", left)
	}
	_ = s.Close()
	if s.SetPeerRelayRegion(peer, three.RegionID) == nil {
		t.Fatal("a pin was accepted after Close")
	}
}

// A peer heard on its route's region keeps the route fresh past the TTL (the flow works only through it); heard on
// another region it does not, and the route expires back to the home.
func TestHeardOnRenewsTheReplyRoute(t *testing.T) {
	peer := key.NewNode().Public()
	s, one, two, three := startRoutesServer(t, 400*time.Millisecond, peer)
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public(), Home: one.RegionID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerRelayRegion(peer, two.RegionID); err != nil {
		t.Fatal(err)
	}
	for range 14 { // 1.4 s, three TTLs
		s.lb.heardOn(peer, two.RegionID)
		time.Sleep(100 * time.Millisecond)
	}
	awaitRegion(t, s, peer, two.RegionID, time.Second)
	for range 14 {
		s.lb.heardOn(peer, three.RegionID)
		time.Sleep(100 * time.Millisecond)
	}
	awaitRegion(t, s, peer, one.RegionID, time.Second)
}

// A machine that reports no home drops what its peers held as its home; a route pinned since stays.
func TestSignedHomeClearedByTheServerFallsBackToTheEnginesOwn(t *testing.T) {
	peer, pinned := key.NewNode().Public(), key.NewNode().Public()
	s, one, two, three := startRoutesServer(t, time.Hour, peer, pinned)
	disco := map[key.NodePublic]key.DiscoPublic{peer: key.NewDisco().Public(), pinned: key.NewDisco().Public()}
	renew := func(home tailcfg.DERPRegionID) {
		t.Helper()
		if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: disco[peer], Home: home}, pinned: {Disco: disco[pinned], Home: home}}); err != nil {
			t.Fatal(err)
		}
	}
	renew(one.RegionID)
	awaitRegion(t, s, peer, one.RegionID, time.Second)
	if err := s.SetPeerRelayRegion(pinned, three.RegionID); err != nil {
		t.Fatal(err)
	}
	renew(0)
	awaitRegion(t, s, peer, two.RegionID, time.Second) // this engine's own home answers
	awaitRegion(t, s, pinned, three.RegionID, time.Second)
}

// A route that ran out (the node went back to the home) is made live again by the peer's next packet on its region.
func TestHeardOnRevivesAnExpiredReplyRoute(t *testing.T) {
	peer := key.NewNode().Public()
	s, one, two, _ := startRoutesServer(t, 200*time.Millisecond, peer)
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: {Disco: key.NewDisco().Public(), Home: one.RegionID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerRelayRegion(peer, two.RegionID); err != nil {
		t.Fatal(err)
	}
	awaitRegion(t, s, peer, one.RegionID, 5*time.Second)
	s.lb.heardOn(peer, two.RegionID)
	awaitRegion(t, s, peer, two.RegionID, time.Second)
	awaitRegion(t, s, peer, one.RegionID, 5*time.Second) // and, unheard again, runs out again: a timer is pending
}

// A signed home whose relay this engine no longer has is not reinstalled on a peer by later renewals (the server keeps
// naming a decommissioned relay): the peer is reached on this engine's own home.
func TestSignedHomeOfAWithdrawnRelayIsNotReinstalled(t *testing.T) {
	peer := key.NewNode().Public()
	s, one, two, _ := startRoutesServer(t, time.Hour, peer)
	contact := PeerContact{Disco: key.NewDisco().Public(), Home: one.RegionID}
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: contact}); err != nil {
		t.Fatal(err)
	}
	awaitRegion(t, s, peer, one.RegionID, time.Second)
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{two}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerContacts(map[key.NodePublic]PeerContact{peer: contact}); err != nil { // the unchanged renewal
		t.Fatal(err)
	}
	awaitRegion(t, s, peer, two.RegionID, time.Second)
}

// A reply route racing a home change never rewrites the stored home; the peer ends where the later of the two says.
func TestReplyRouteRacingAHomeChangeKeepsTheHome(t *testing.T) {
	const peers = 40
	keys := make([]key.NodePublic, peers)
	contacts := map[key.NodePublic]PeerContact{}
	for i := range keys {
		keys[i] = key.NewNode().Public()
		contacts[keys[i]] = PeerContact{Disco: key.NewDisco().Public()}
	}
	s, one, two, _ := startRoutesServer(t, time.Hour, keys...)
	if err := s.SetPeerContacts(contacts); err != nil {
		t.Fatal(err)
	}
	for _, peer := range keys {
		c := contacts[peer]
		c.Home = one.RegionID
		contacts[peer] = c
		next := maps.Clone(contacts)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.lb.setPeerRelayRegion(peer, two.RegionID) }()
		go func() {
			defer wg.Done()
			if err := s.SetPeerContacts(next); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		if got := peerHome(s, peer); got != one.RegionID {
			t.Fatalf("stored home %d, want %d", got, one.RegionID)
		}
		// Whichever of the two took the locks last decides: a route set after the new home outranks it while fresh, one set
		// before it is dropped.
		s.lb.mu.Lock()
		route := s.lb.replyRoutes[peer]
		s.lb.mu.Unlock()
		want := one.RegionID
		if route != nil {
			want = two.RegionID
		}
		if got := s.lb.peerDERPRegion(peer); got != want {
			t.Fatalf("peer is on region %d, want %d (route held: %v)", got, want, route != nil)
		}
	}
}
