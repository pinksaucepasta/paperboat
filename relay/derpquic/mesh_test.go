package derpquic

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
	"go4.org/mem"
	"tailscale.com/derp"
	"tailscale.com/types/key"
)

// meshNode is one relay of a two-relay fixture: its own node id, certificate (so its own pin) and UDP socket, sharing
// the grant signing key with the other so each verifies grants the other's carriers hold.
type meshNode struct {
	t      *testing.T
	f      *fixture
	server *Server
	mesh   *Mesh
	addr   string
	cert   tls.Certificate
	stop   func()
}

func (n *meshNode) peer() nodemesh.Peer {
	_, port, _ := net.SplitHostPort(n.addr)
	p, _ := strconv.Atoi(port)
	return nodemesh.Peer{NodeID: n.f.verifier.NodeID, Region: "test", Host: "127.0.0.1", Port: uint16(p), SPKISHA256: nodemesh.SPKIPin(n.cert.Leaf)}
}

// startMeshNode starts a relay with a mesh on address (a port again for a restart).
func startMeshNode(t *testing.T, f *fixture, address string) *meshNode {
	t.Helper()
	socket, err := net.ListenPacket("udp4", address)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	mesh, err := NewMesh(MeshConfig{Cert: f.tls.Certificates[0], RetryBase: 20 * time.Millisecond, RetryMax: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.SetMesh(mesh); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, socket, f.tls) }()
	eventually(t, func() bool { server.mu.Lock(); defer server.mu.Unlock(); return server.listener != nil })
	n := &meshNode{t: t, f: f, server: server, mesh: mesh, addr: socket.LocalAddr().String(), cert: f.tls.Certificates[0]}
	var once sync.Once
	n.stop = func() {
		once.Do(func() {
			cancel()
			server.Close()
			select {
			case <-done:
			case <-time.After(waitBound):
				t.Error("Serve did not stop")
			}
			server.Wait()
			socket.Close()
		})
	}
	t.Cleanup(n.stop)
	return n
}

// relayFixtures returns two fixtures of relays "relay" (dials) and "relay-b" (accepts), one grant signer.
func relayFixtures(t *testing.T) (*fixture, *fixture) {
	a := newFixture(t)
	b := *a
	b.verifier.NodeID = "relay-b"
	cert := certificate(t)
	b.tls = &tls.Config{Certificates: []tls.Certificate{cert}}
	b.roots = x509.NewCertPool()
	b.roots.AddCert(cert.Leaf)
	return a, &b
}

// meshPair starts both relays and links them; link false leaves the directories empty.
func meshPair(t *testing.T, link bool) (*meshNode, *meshNode) {
	t.Helper()
	fa, fb := relayFixtures(t)
	a, b := startMeshNode(t, fa, "127.0.0.1:0"), startMeshNode(t, fb, "127.0.0.1:0")
	if link {
		linkNodes(t, a, b)
	}
	return a, b
}

func linkNodes(t *testing.T, a, b *meshNode) {
	t.Helper()
	if err := b.mesh.SetPeers([]nodemesh.Peer{a.peer()}); err != nil {
		t.Fatal(err)
	}
	if err := a.mesh.SetPeers([]nodemesh.Peer{b.peer()}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return a.mesh.stats().Links == 1 && b.mesh.stats().Links == 1 })
}

func (m *Mesh) holder(k key.NodePublic) *meshLink {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.holders[k]
}

func (m *Mesh) holderCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.holders)
}

// pairedClients connects a client on a and one on b whose grants pair them (a dials, b accepts).
func pairedClients(t *testing.T, a, b *meshNode) (ka, kb key.NodePublic, ca, cb *Client) {
	t.Helper()
	ka, kb = key.NewNode().Public(), key.NewNode().Public()
	certA, certB := certificate(t), certificate(t)
	ga, gb := a.f.grant(ka, certA, "a"), b.f.grant(kb, certB, "b")
	pairScopes(&ga, &gb)
	ca, cb = a.f.client(a.addr, certA, ga), b.f.client(b.addr, certB, gb)
	connect(t, ca)
	connect(t, cb)
	return ka, kb, ca, cb
}

func payloadOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

// deliverEventually sends payload until the receiver gets it: the first packet of a flow is lost while the
// destination's relay fetches the sender's grant, and the transports above retransmit.
func deliverEventually(t *testing.T, from *Client, dst key.NodePublic, to *Client, src key.NodePublic, payload []byte) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		for {
			m, _, err := to.RecvDetail()
			if err != nil {
				got <- err
				return
			}
			if p, ok := m.(derp.ReceivedPacket); ok && p.Source == src && bytes.Equal(p.Data, payload) {
				got <- nil
				return
			}
		}
	}()
	tick, deadline := time.NewTicker(20*time.Millisecond), time.After(waitBound)
	defer tick.Stop()
	for {
		select {
		case err := <-got:
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-tick.C:
			from.Send(dst, payload)
		case <-deadline:
			t.Fatal("packet was not delivered over the mesh")
		}
	}
}

// silent reports whether nothing arrives at c within d.
func silent(c *Client, d time.Duration) bool {
	got := make(chan struct{}, 1)
	go func() {
		if _, _, err := c.RecvDetail(); err == nil {
			got <- struct{}{}
		}
	}()
	select {
	case <-got:
		return false
	case <-time.After(d):
		return true
	}
}

func TestMeshForwardsPacketToHolderNode(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb, ca, cb := pairedClients(t, a, b)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil && b.mesh.holder(ka) != nil })
	data := payloadOf(1312) // two fragments
	deliverEventually(t, ca, kb, cb, ka, data)
	deliverEventually(t, cb, ka, ca, kb, payloadOf(300))
	if got := a.server.Snapshot().BytesForwarded; got < 300 || b.server.Snapshot().BytesForwarded < uint64(len(data)) {
		t.Fatalf("each relay counts the payload it delivered: a=%d b=%d", got, b.server.Snapshot().BytesForwarded)
	}
	stats := a.mesh.stats()
	if stats.Forwarded == 0 || stats.BytesOut < uint64(len(data)) || b.mesh.stats().Accepted == 0 {
		t.Fatalf("mesh counters: a=%+v b=%+v", stats, b.mesh.stats())
	}
}

func TestMeshLocalWinsOverForward(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	certA, certB := certificate(t), certificate(t)
	ga, gbA, gbB := a.f.grant(ka, certA, "a"), a.f.grant(kb, certB, "b"), b.f.grant(kb, certB, "b")
	pairScopes(&ga, &gbA)
	gbB.Memberships = gbA.Memberships
	ca := a.f.client(a.addr, certA, ga)
	localB := a.f.client(a.addr, certB, gbA)  // kb is connected here ...
	remoteB := b.f.client(b.addr, certB, gbB) // ... and on the other relay
	connect(t, ca)
	connect(t, localB)
	connect(t, remoteB)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	deliverEventually(t, ca, kb, localB, ka, payloadOf(200))
	if !silent(remoteB, 300*time.Millisecond) || a.mesh.stats().Forwarded != 0 {
		t.Fatal("a packet for a locally connected peer went over the mesh")
	}
}

func TestMeshNeverReforwards(t *testing.T) {
	a, b := meshPair(t, true)
	_, _, ca, _ := pairedClients(t, a, b)
	x := key.NewNode().Public() // connected nowhere; each relay believes the other holds it
	a.mesh.mu.Lock()
	a.mesh.holders[x] = a.mesh.links["relay-b"]
	a.mesh.mu.Unlock()
	b.mesh.mu.Lock()
	b.mesh.holders[x] = b.mesh.links["relay"]
	b.mesh.mu.Unlock()
	eventually(t, func() bool { ca.Send(x, payloadOf(100)); return b.mesh.loopRefused.Load() > 0 })
	time.Sleep(200 * time.Millisecond)
	if b.mesh.forwarded.Load() != 0 || a.mesh.received.Load() != 0 {
		t.Fatalf("a packet that arrived over the mesh was forwarded again: b forwarded %d, a received %d", b.mesh.forwarded.Load(), a.mesh.received.Load())
	}
}

func TestMeshNeedGrantAuthorizesRemoteSender(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb, ca, cb := pairedClients(t, a, b)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	b.mesh.mu.Lock()
	early := b.mesh.remote.len()
	b.mesh.mu.Unlock()
	if early != 0 {
		t.Fatal("grant cached before any packet")
	}
	deliverEventually(t, ca, kb, cb, ka, payloadOf(500))
	b.mesh.mu.Lock()
	cached := b.mesh.remote.len()
	b.mesh.mu.Unlock()
	if cached != 1 || b.mesh.drops[dropNoGrant].Load() == 0 {
		t.Fatalf("the first packet of a flow is dropped while the grant is fetched: cached %d, no-grant drops %d", cached, b.mesh.drops[dropNoGrant].Load())
	}
}

func TestMeshUnpairedSenderDropped(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	certA, certB := certificate(t), certificate(t)
	ca := a.f.client(a.addr, certA, a.f.grant(ka, certA, "a")) // no memberships: not paired
	cb := b.f.client(b.addr, certB, b.f.grant(kb, certB, "b"))
	connect(t, ca)
	connect(t, cb)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	eventually(t, func() bool { ca.Send(kb, payloadOf(100)); return b.mesh.drops[dropUnpaired].Load() > 0 })
	if !silent(cb, 200*time.Millisecond) || b.server.Snapshot().BytesForwarded != 0 {
		t.Fatal("a sender not paired with the destination was delivered")
	}
}

func TestMeshPeerGonePropagates(t *testing.T) {
	a, b := meshPair(t, true)
	_, kb, _, cb := pairedClients(t, a, b)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	cb.Close()
	eventually(t, func() bool { return a.mesh.holder(kb) == nil })
}

func TestMeshReconnectRefloodsAndClears(t *testing.T) {
	a, b := meshPair(t, true)
	_, kb, _, _ := pairedClients(t, a, b)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	if err := a.mesh.SetPeers(nil); err != nil { // the link ends: everything the peer held is cleared
		t.Fatal(err)
	}
	eventually(t, func() bool { return a.mesh.holderCount() == 0 && a.mesh.stats().Links == 0 })
	if err := a.mesh.SetPeers([]nodemesh.Peer{b.peer()}); err != nil { // the link returns: the snapshot refloods
		t.Fatal(err)
	}
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	// A link broken by the network (not by the directory) is redialled by itself.
	a.mesh.mu.RLock()
	l := a.mesh.links["relay-b"]
	a.mesh.mu.RUnlock()
	l.close()
	eventually(t, func() bool {
		a.mesh.mu.RLock()
		defer a.mesh.mu.RUnlock()
		n := a.mesh.links["relay-b"]
		return n != nil && n != l && a.mesh.holders[kb] != nil
	})
}

func TestMeshPinMismatchRefused(t *testing.T) {
	a, b := meshPair(t, false)
	wrong := b.peer()
	wrong.SPKISHA256 = nodemesh.SPKIPin(certificate(t).Leaf)
	if err := b.mesh.SetPeers([]nodemesh.Peer{a.peer()}); err != nil {
		t.Fatal(err)
	}
	if err := a.mesh.SetPeers([]nodemesh.Peer{wrong}); err != nil { // a dials b expecting another certificate
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if a.mesh.stats().Links != 0 || b.mesh.stats().Links != 0 {
		t.Fatal("a link formed to a relay that does not present the pinned certificate")
	}
	// b's directory lacks a: b refuses a's correctly pinned dial.
	if err := a.mesh.SetPeers([]nodemesh.Peer{b.peer()}); err != nil {
		t.Fatal(err)
	}
	if err := b.mesh.SetPeers(nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if a.mesh.stats().Links != 0 || b.mesh.stats().Links != 0 {
		t.Fatal("a relay outside the directory was accepted")
	}
}

func TestMeshDoesNotDoubleChargeAccountBudget(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb, ca, cb := pairedClients(t, a, b)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	deliverEventually(t, ca, kb, cb, ka, payloadOf(1000)) // the grant is cached now
	a.server.TakeAccountDemand()
	b.server.TakeAccountDemand()
	before := b.server.Snapshot().BytesForwarded
	const packets, size = 40, 1000
	for range packets {
		if err := ca.Send(kb, payloadOf(size)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool { return b.server.Snapshot().BytesForwarded-before >= packets*size/2 })
	if got := a.server.TakeAccountDemand()["account"].Offered; got < packets*size {
		t.Fatalf("the ingress relay charges the account for what the sender offered: %d", got)
	}
	if got := b.server.TakeAccountDemand()["account"].Offered; got >= packets*size/4 {
		t.Fatalf("the delivering relay charged the account budget again: %d", got)
	}
	if a.server.Snapshot().BytesForwarded != 0 {
		t.Fatal("transit bytes were counted as forwarded on the ingress relay")
	}
}

func TestMeshDrainKeepsForwardingUntilDeadline(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb, ca, cb := pairedClients(t, a, b)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	deadline := time.Now().Add(2 * time.Second)
	b.server.Drain(deadline)
	deliverEventually(t, ca, kb, cb, ka, payloadOf(200))
	if a.mesh.stats().Links != 1 || a.mesh.holder(kb) == nil {
		t.Fatal("a draining relay lost its link or withdrew its carriers before the deadline")
	}
	waitPastExpiry(t, deadline.Unix()+1)
	eventually(t, func() bool { return a.mesh.holder(kb) == nil }) // closeDrained withdrew everything
}

func TestMeshRestartRecovers(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb, ca, cb := pairedClients(t, a, b)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	deliverEventually(t, ca, kb, cb, ka, payloadOf(200))
	addr, fb := b.addr, b.f
	b.stop()
	eventually(t, func() bool { return a.mesh.stats().Links == 0 && a.mesh.holderCount() == 0 })
	b = startMeshNode(t, fb, addr)
	if err := b.mesh.SetPeers([]nodemesh.Peer{a.peer()}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return a.mesh.stats().Links == 1 })
	connect(t, cb)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil })
	deliverEventually(t, ca, kb, cb, ka, payloadOf(300))
}

// randomPublic is a node key without the cost of generating a key pair (20,000 of them under -race).
func randomPublic() key.NodePublic {
	var raw [32]byte
	rand.Read(raw[:])
	return key.NodePublicFromRaw32(mem.B(raw[:]))
}

func TestMeshHolderMapBounded(t *testing.T) {
	m, err := NewMesh(MeshConfig{Cert: certificate(t)})
	if err != nil {
		t.Fatal(err)
	}
	m.holderLimit = 1000
	l := &meshLink{m: m}
	const peers, perFrame = 20000, 500
	announced := make([]string, peers)
	for i := range announced {
		announced[i] = KeyString(randomPublic())
	}
	for range 2 { // the same peers announce twice: a held key is not a new entry, an over-limit one is dropped each time
		for sent := 0; sent < peers; sent += perFrame {
			if err := m.handle(l, meshFrame{Kind: meshPresent, Keys: announced[sent : sent+perFrame]}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := m.holderCount(); got != 1000 || m.drops[dropHolderFull].Load() != 2*(peers-1000) {
		t.Fatalf("holder map holds %d entries after %d announcements twice, dropped %d", got, peers, m.drops[dropHolderFull].Load())
	}
	cache := newRemoteCache(100)
	for range peers {
		cache.put(&remoteSource{route: randomPublic()})
	}
	if cache.len() != 100 || len(cache.index) != 100 {
		t.Fatalf("remote grant cache holds %d entries", cache.len())
	}
}

func TestMeshQueueFullDrops(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMesh(MeshConfig{Cert: f.tls.Certificates[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMesh(m); err != nil {
		t.Fatal(err)
	}
	l := &meshLink{m: m, out: make(chan meshOut, 2)}
	p, dst := &peerConn{key: key.NewNode().Public()}, key.NewNode().Public()
	s.peers[p.key], m.holders[dst] = p, l
	for range 5 {
		if !s.forwardLocked(p, dst, packet{peer: dst, data: []byte("x")}) {
			t.Fatal("a held destination is forwarded, queue full or not")
		}
	}
	if len(l.out) != 2 || m.drops[dropQueueFull].Load() != 3 || m.unreachable.Load() != 3 {
		t.Fatalf("queued %d, dropped %d, unreachable %d", len(l.out), m.drops[dropQueueFull].Load(), m.unreachable.Load())
	}
}
