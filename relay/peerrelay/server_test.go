// Copyright (c) Paperboat contributors
// Portions adapted from Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package peerrelay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"tailscale.com/derp"
	"tailscale.com/disco"
	"tailscale.com/net/packet"
	"tailscale.com/types/key"
)

type testFixture struct {
	afterHandle atomic.Pointer[func(context.Context, []byte, error) ([]byte, error)]
	t           *testing.T
	server      *derpquic.Server
	relay       *Server
	active      atomic.Pointer[Server]
	config      Config
	a, b        *derpquic.Client
	da, db      key.DiscoPrivate
	service     key.NodePublic
	ga, gb      derpquic.Grant
	reconnectA  func(derpquic.Grant) *derpquic.Client
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if e != nil {
		t.Fatal(e)
	}
	tmpl := &x509.Certificate{SerialNumber: serial, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	raw, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if e != nil {
		t.Fatal(e)
	}
	leaf, e := x509.ParseCertificate(raw)
	if e != nil {
		t.Fatal(e)
	}
	return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: priv, Leaf: leaf}
}
func signGrant(t *testing.T, priv ed25519.PrivateKey, g derpquic.Grant) string {
	t.Helper()
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"paperboat-relay-grant+jwt","kid":"test"}`))
	b, e := json.Marshal(g)
	if e != nil {
		t.Fatal(e)
	}
	body := h + "." + base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(until) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func newFixture(t *testing.T, mutate func(*derpquic.Grant, *derpquic.Grant)) *testFixture {
	t.Helper()
	f := &testFixture{t: t, da: key.NewDisco(), db: key.NewDisco(), service: key.NewNode().Public()}
	serviceDisco := key.NewDisco()
	descriptor := derpquic.ServiceDescriptor{WireGuardPublicKey: derpquic.KeyString(f.service), DiscoPublicKey: derpquic.DiscoKeyString(serviceDisco.Public()), VirtualAddress: "fd7a:115c:a1e0::100"}
	reserve, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if e != nil {
		t.Fatal(e)
	}
	port := uint16(reserve.LocalAddr().(*net.UDPAddr).Port)
	reserve.Close()
	f.config = Config{Service: descriptor, DiscoPrivate: serviceDisco, Port: port, Addresses: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)}}
	f.relay, e = New(f.config)
	if e != nil {
		t.Fatal(e)
	}
	f.active.Store(f.relay)
	t.Cleanup(func() { f.active.Load().Close() })
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	f.server, e = derpquic.NewServer(derpquic.Verifier{Issuer: "authority", NodeID: "relay", NodeGeneration: 1, ProcessEpoch: "epoch", Keys: map[string]ed25519.PublicKey{"test": pub}})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.server.SetControlService(descriptor, func(ctx context.Context, r derpquic.ControlRequest) ([]byte, error) {
		reply, err := f.active.Load().Handle(ctx, r)
		if hook := f.afterHandle.Load(); hook != nil {
			return (*hook)(ctx, reply, err)
		}
		return reply, err
	}); e != nil {
		t.Fatal(e)
	}
	serverCert := testCertificate(t)
	roots := x509.NewCertPool()
	roots.AddCert(serverCert.Leaf)
	socket, e := net.ListenPacket("udp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.server.Serve(ctx, socket, &tls.Config{Certificates: []tls.Certificate{serverCert}}) }()
	t.Cleanup(func() {
		cancel()
		f.server.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("DERP server did not stop")
		}
		f.server.Wait()
		socket.Close()
	})
	ca, cb := testCertificate(t), testCertificate(t)
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	now := time.Now().Unix()
	grant := func(k key.NodePublic, d key.DiscoPrivate, c tls.Certificate, id string) derpquic.Grant {
		fp := sha256.Sum256(c.Certificate[0])
		return derpquic.Grant{Version: 1, Issuer: "authority", Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: 1, AccountID: "account", EndpointID: id, WireGuardPublicKey: derpquic.KeyString(k), DiscoPublicKey: derpquic.DiscoKeyString(d.Public()), CertificateFingerprint: hex.EncodeToString(fp[:]), QUICPublicKey: base64.RawURLEncoding.EncodeToString(c.Leaf.PublicKey.(ed25519.PublicKey)), NodeID: "relay", NodeGeneration: 1, ProcessEpoch: "epoch", PeerRelay: &descriptor}
	}
	f.ga, f.gb = grant(ka, f.da, ca, "a"), grant(kb, f.db, cb, "b")
	scope := derpquic.Scope{ResourceKind: "machine_access", ResourceID: "machine", ResourceGeneration: 1, Capability: "terminal", Direction: "dial", Port: 443, ExpiresAt: now + 60}
	f.ga.Peers = []derpquic.Peer{{WireGuardPublicKey: f.gb.WireGuardPublicKey, DiscoPublicKey: f.gb.DiscoPublicKey, Scopes: []derpquic.Scope{scope}}}
	scope.Direction = "accept"
	f.gb.Peers = []derpquic.Peer{{WireGuardPublicKey: f.ga.WireGuardPublicKey, DiscoPublicKey: f.ga.DiscoPublicKey, Scopes: []derpquic.Scope{scope}}}
	if mutate != nil {
		mutate(&f.ga, &f.gb)
	}
	client := func(cert tls.Certificate, g derpquic.Grant) *derpquic.Client {
		token := signGrant(t, priv, g)
		c := derpquic.NewClient(derpquic.ClientConfig{Address: socket.LocalAddr().String(), TLS: &tls.Config{RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}}, Credential: func(context.Context) (string, error) { return token, nil }})
		t.Cleanup(func() { c.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if e := c.Connect(ctx); e != nil {
			t.Fatal(e)
		}
		return c
	}
	f.a, f.b = client(ca, f.ga), client(cb, f.gb)
	f.reconnectA = func(g derpquic.Grant) *derpquic.Client { return client(ca, g) }
	return f
}
func allocationMessage(d key.DiscoPrivate, service key.DiscoPublic, a, b key.DiscoPublic, generation uint32) []byte {
	request := &disco.AllocateUDPRelayEndpointRequest{ClientDisco: [2]key.DiscoPublic{a, b}, Generation: generation}
	out := append([]byte(nil), disco.Magic...)
	out = d.Public().AppendTo(out)
	return append(out, d.Shared(service).Seal(request.AppendMarshal(nil))...)
}
func (f *testFixture) allocate(generation uint32) *disco.AllocateUDPRelayEndpointResponse {
	f.t.Helper()
	payload := allocationMessage(f.da, f.config.DiscoPrivate.Public(), f.da.Public(), f.db.Public(), generation)
	if e := f.a.SendControl(f.service, payload); e != nil {
		f.t.Fatal(e)
	}
	type result struct {
		packet derp.ReceivedPacket
		err    error
	}
	done := make(chan result, 1)
	go func() {
		for {
			m, _, e := f.a.RecvDetail()
			if e != nil {
				done <- result{err: e}
				return
			}
			if p, ok := m.(derp.ReceivedPacket); ok {
				done <- result{packet: p}
				return
			}
		}
	}()
	var received derp.ReceivedPacket
	select {
	case r := <-done:
		if r.err != nil {
			f.t.Fatal(r.err)
		}
		received = r.packet
	case <-time.After(3 * time.Second):
		f.a.Close()
		f.t.Fatal("allocation response timed out")
	}
	header := len(disco.Magic) + 32
	if received.Source != f.service || len(received.Data) < header || !bytes.Equal(received.Data[:header], append([]byte(disco.Magic), f.config.DiscoPrivate.Public().AppendTo(nil)...)) {
		f.t.Fatal("allocation response identity differs")
	}
	clear, ok := f.da.Shared(f.config.DiscoPrivate.Public()).Open(received.Data[header:])
	if !ok {
		f.t.Fatal("allocation response cannot decrypt")
	}
	message, e := disco.Parse(clear)
	if e != nil {
		f.t.Fatal(e)
	}
	response, ok := message.(*disco.AllocateUDPRelayEndpointResponse)
	if !ok || response.Generation != generation || response.ClientDisco != key.NewSortedPairOfDiscoPublic(f.da.Public(), f.db.Public()).Get() {
		f.t.Fatal("allocation response binding differs")
	}
	return response
}

// UDP binding follows upstream net/udprelay/server_test.go's BSD-3-Clause
// challenge/answer flow. Payloads below are test data, not WireGuard encryption.
type udpClient struct {
	conn           *net.UDPConn
	local          key.DiscoPrivate
	remote, server key.DiscoPublic
	vni            uint32
}

func newUDPClient(t *testing.T, endpoint netip.AddrPort, local key.DiscoPrivate, remote, server key.DiscoPublic, vni uint32) *udpClient {
	t.Helper()
	c, e := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(endpoint))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	return &udpClient{c, local, remote, server, vni}
}
func (c *udpClient) send(t *testing.T, control bool, b []byte) {
	t.Helper()
	h := packet.GeneveHeader{Control: control, Protocol: packet.GeneveProtocolWireGuard}
	if control {
		h.Protocol = packet.GeneveProtocolDisco
	}
	h.VNI.Set(c.vni)
	out := make([]byte, packet.GeneveFixedHeaderLength)
	if e := h.Encode(out); e != nil {
		t.Fatal(e)
	}
	out = append(out, b...)
	if _, e := c.conn.Write(out); e != nil {
		t.Fatal(e)
	}
}
func (c *udpClient) read(t *testing.T) []byte {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 4096)
	n, e := c.conn.Read(b)
	if e != nil {
		t.Fatal(e)
	}
	var h packet.GeneveHeader
	if e = h.Decode(b[:n]); e != nil || h.VNI.Get() != c.vni {
		t.Fatal("invalid UDP relay header")
	}
	return b[packet.GeneveFixedHeaderLength:n]
}
func (c *udpClient) bind(t *testing.T) {
	t.Helper()
	common := disco.BindUDPRelayEndpointCommon{VNI: c.vni, Generation: 1, RemoteKey: c.remote}
	seal := func(m disco.Message) []byte {
		b := append([]byte(nil), disco.Magic...)
		b = c.local.Public().AppendTo(b)
		return append(b, c.local.Shared(c.server).Seal(m.AppendMarshal(nil))...)
	}
	c.send(t, true, seal(&disco.BindUDPRelayEndpoint{BindUDPRelayEndpointCommon: common}))
	reply := c.read(t)
	header := len(disco.Magic) + 32
	if len(reply) < header {
		t.Fatal("short bind reply")
	}
	clear, ok := c.local.Shared(c.server).Open(reply[header:])
	if !ok {
		t.Fatal("bind challenge cannot decrypt")
	}
	m, e := disco.Parse(clear)
	if e != nil {
		t.Fatal(e)
	}
	challenge, ok := m.(*disco.BindUDPRelayEndpointChallenge)
	if !ok || challenge.VNI != common.VNI || challenge.Generation != common.Generation || challenge.RemoteKey != common.RemoteKey {
		t.Fatal("invalid bind challenge")
	}
	answer := &disco.BindUDPRelayEndpointAnswer{BindUDPRelayEndpointCommon: common}
	answer.Challenge = challenge.Challenge
	c.send(t, true, seal(answer))
}
func (f *testFixture) bound(response *disco.AllocateUDPRelayEndpointResponse) (*udpClient, *udpClient) {
	f.t.Helper()
	a := newUDPClient(f.t, response.AddrPorts[0], f.da, f.db.Public(), response.ServerDisco, response.VNI)
	b := newUDPClient(f.t, response.AddrPorts[0], f.db, f.da.Public(), response.ServerDisco, response.VNI)
	a.bind(f.t)
	b.bind(f.t)
	waitFor(f.t, func() bool {
		sessions := f.active.Load().udp.GetSessions()
		return len(sessions) == 1 && sessions[0].Client1.Endpoint.IsValid() && sessions[0].Client2.Endpoint.IsValid()
	})
	return a, b
}
func noUDP(t *testing.T, c *udpClient) {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	if n, e := c.conn.Read(make([]byte, 4096)); e == nil || n != 0 {
		t.Fatal("denied UDP packet arrived")
	}
}

func TestControlToUDPForwardingRevocation(t *testing.T) {
	f := newFixture(t, nil)
	first := f.allocate(7)
	a, b := f.bound(first)
	payload := bytes.Repeat([]byte{4}, 1312)
	a.send(t, false, payload)
	if got := b.read(t); !bytes.Equal(got, payload) {
		t.Fatal("forwarded bytes changed")
	}
	repeated := f.allocate(8)
	if repeated.VNI != first.VNI || repeated.LamportID != first.LamportID {
		t.Fatal("replayed pair request multiplied allocation")
	}
	before := f.relay.Snapshot().AuthorizedPackets
	forged := newUDPClient(t, first.AddrPorts[0], key.NewDisco(), f.db.Public(), first.ServerDisco, first.VNI)
	forged.send(t, false, payload)
	noUDP(t, b)
	if f.relay.Snapshot().AuthorizedPackets != before {
		t.Fatal("forged source consumed account forwarding quota")
	}
	a.send(t, false, make([]byte, 2049))
	noUDP(t, b)
	f.server.Revoke("account", "a", 2)
	a.send(t, false, payload)
	noUDP(t, b)
	waitFor(t, func() bool { return f.relay.Snapshot().Allocations == 0 })
}
func TestControlToUDPRestart(t *testing.T) {
	f := newFixture(t, nil)
	first := f.allocate(1)
	a, b := f.bound(first)
	payload := []byte("before restart")
	a.send(t, false, payload)
	b.read(t)
	f.relay.Close()
	replacement, e := New(f.config)
	if e != nil {
		t.Fatal(e)
	}
	f.relay = replacement
	f.active.Store(replacement)
	// A fresh server has no bound allocation, even though old clients retain a VNI.
	a.send(t, false, payload)
	noUDP(t, b)
	second := f.allocate(2)
	if second.ServerDisco == first.ServerDisco {
		t.Fatal("UDP server restart retained discovery private identity")
	}
	a2, b2 := f.bound(second)
	a2.send(t, false, payload)
	if got := b2.read(t); !bytes.Equal(got, payload) {
		t.Fatal("restart forwarding failed")
	}
}
func TestAllocationDeniedSignedBindings(t *testing.T) {
	for _, mode := range []string{"source_discovery", "peer_discovery", "scope", "service"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, func(a, b *derpquic.Grant) {
				switch mode {
				case "source_discovery":
					a.DiscoPublicKey = derpquic.DiscoKeyString(key.NewDisco().Public())
					b.Peers[0].DiscoPublicKey = a.DiscoPublicKey
				case "peer_discovery":
					a.Peers[0].DiscoPublicKey = derpquic.DiscoKeyString(key.NewDisco().Public())
				case "scope":
					b.Peers[0].Scopes[0].ResourceGeneration++
				case "service":
					copy := *a.PeerRelay
					copy.VirtualAddress = "fd7a:115c:a1e0::101"
					a.PeerRelay = &copy
				}
			})
			payload := allocationMessage(f.da, f.config.DiscoPrivate.Public(), f.da.Public(), f.db.Public(), 1)
			if e := f.a.SendControl(f.service, payload); e != nil {
				t.Fatal(e)
			}
			waitFor(t, func() bool { return f.server.Snapshot().Dropped > 0 })
			if f.relay.Snapshot().Allocations != 0 {
				t.Fatal("denied request allocated a UDP endpoint")
			}
		})
	}
}
func TestAllocationExpiryLifetimeAndRateBound(t *testing.T) {
	f := newFixture(t, func(a, b *derpquic.Grant) { b.ExpiresAt = time.Now().Add(2 * time.Second).Unix() })
	response := f.allocate(1)
	remaining := time.Until(time.Unix(f.gb.ExpiresAt, 0))
	if response.BindLifetime <= 0 || response.SteadyStateLifetime <= 0 || response.BindLifetime > remaining+100*time.Millisecond || response.SteadyStateLifetime > remaining+100*time.Millisecond {
		t.Fatal("response lifetime exceeds shorter peer grant")
	}
	a, b := f.bound(response)
	payload := allocationMessage(f.da, f.config.DiscoPrivate.Public(), f.da.Public(), f.db.Public(), 2)
	for range 20 {
		if e := f.a.SendControl(f.service, payload); e != nil {
			t.Fatal(e)
		}
	}
	waitFor(t, func() bool { return f.relay.Snapshot().Denied > 0 })
	if f.relay.Snapshot().Allocations != 1 {
		t.Fatal("allocation request burst escaped pair bound")
	}
	waitFor(t, func() bool { return time.Now().Unix() >= f.gb.ExpiresAt })
	a.send(t, false, []byte("expired"))
	noUDP(t, b)
	waitFor(t, func() bool { return f.relay.Snapshot().Allocations == 0 })
}
func TestForwardRateBurstRecovery(t *testing.T) {
	var r rate
	now := time.Now()
	for range 256 {
		if !r.allow(now, 4096, 256) {
			t.Fatal("burst ended early")
		}
	}
	if r.allow(now, 4096, 256) {
		t.Fatal("burst exceeded bound")
	}
	if !r.allow(now.Add(time.Second), 4096, 256) {
		t.Fatal("budget failed to recover")
	}
}

func TestAllocationReplyFencedWhenOtherPeerRevoked(t *testing.T) {
	f := newFixture(t, nil)
	entered := make(chan error, 1)
	release := make(chan struct{}, 1)
	defer close(release)
	hook := func(ctx context.Context, reply []byte, err error) ([]byte, error) {
		entered <- err
		select {
		case <-release:
			return reply, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.afterHandle.Store(&hook)
	received := make(chan error, 1)
	go func() {
		for {
			message, _, err := f.a.RecvDetail()
			if err != nil {
				received <- err
				return
			}
			if _, ok := message.(derp.ReceivedPacket); ok {
				received <- nil
				return
			}
		}
	}()
	payload := allocationMessage(f.da, f.config.DiscoPrivate.Public(), f.da.Public(), f.db.Public(), 1)
	if err := f.a.SendControl(f.service, payload); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-entered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not produce allocation")
	}
	if !f.relay.authorizePacket(f.da.Public(), f.db.Public()) {
		t.Fatal("pre-revocation allocation was not authorized")
	}
	before := f.server.Snapshot().Dropped
	f.server.Revoke("account", "b", 2)
	if f.relay.authorizePacket(f.da.Public(), f.db.Public()) {
		t.Fatal("other-peer revocation left active UDP authorization")
	}
	release <- struct{}{}
	waitFor(t, func() bool { return f.server.Snapshot().Dropped > before })
	select {
	case err := <-received:
		if err == nil {
			t.Fatal("stale allocation response delivered after other-peer revocation")
		}
		t.Fatalf("requesting peer unexpectedly closed: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	f.a.Close()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("pending receive did not close")
	}
}

func TestDrainRejectsAllocationWhileExistingUDPContinues(t *testing.T) {
	f := newFixture(t, nil)
	response := f.allocate(1)
	a, b := f.bound(response)
	f.server.Drain(time.Now().Add(2 * time.Second))
	before := f.server.Snapshot().Dropped
	payload := allocationMessage(f.da, f.config.DiscoPrivate.Public(), f.da.Public(), f.db.Public(), 2)
	if err := f.a.SendControl(f.service, payload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.server.Snapshot().Dropped > before })
	if f.relay.Snapshot().Allocations != 1 || f.relay.Snapshot().Admitted != 1 {
		t.Fatal("drain admitted another allocation request")
	}
	data := []byte("active relay drains until deadline")
	a.send(t, false, data)
	if got := b.read(t); !bytes.Equal(got, data) {
		t.Fatal("drain interrupted existing authorized UDP traffic")
	}
}

func TestUDPAllocationSurvivesDERPReconnect(t *testing.T) {
	f := newFixture(t, nil)
	first := f.allocate(1)
	a, b := f.bound(first)
	payload := bytes.Repeat([]byte{4}, 1312)
	f.a.Close()
	waitFor(t, func() bool { return f.server.Snapshot().Connections == 1 })
	time.Sleep(350 * time.Millisecond)
	a.send(t, false, payload)
	if got := b.read(t); !bytes.Equal(got, payload) {
		t.Fatal("data changed during DERP disconnect")
	}
	f.a = f.reconnectA(f.ga)
	a.send(t, false, payload)
	if got := b.read(t); !bytes.Equal(got, payload) {
		t.Fatal("data changed after DERP reconnect")
	}
	next := f.allocate(2)
	if first.VNI != next.VNI || first.LamportID != next.LamportID {
		t.Fatal("DERP reconnect replaced UDP allocation")
	}
	f.relay.mu.Lock()
	var oldLease PairLease
	for _, allocation := range f.relay.allocations {
		oldLease = allocation.lease
	}
	f.relay.mu.Unlock()
	// A compatible signed renewal keeps the allocation.
	f.ga.Generation = 2
	f.a = f.reconnectA(f.ga)
	if _, ok := oldLease.Valid(); !ok {
		t.Fatal("compatible signed renewal invalidated allocation")
	}
	a.send(t, false, payload)
	if got := b.read(t); !bytes.Equal(got, payload) {
		t.Fatal("renewal data changed")
	}
	if err := f.server.Revoke("account", "a", 3); err != nil {
		t.Fatal(err)
	}
	f.ga.Generation = 4
	f.a = f.reconnectA(f.ga)
	if _, ok := oldLease.Valid(); ok {
		t.Fatal("regrant revived revoked allocation lease")
	}
	a.send(t, false, payload)
	noUDP(t, b)
}

func TestUDPAllocationFencedByChangedAuthority(t *testing.T) {
	f := newFixture(t, nil)
	first := f.allocate(1)
	a, b := f.bound(first)
	f.relay.mu.Lock()
	var oldLease PairLease
	for _, entry := range f.relay.allocations {
		oldLease = entry.lease
	}
	f.relay.mu.Unlock()
	f.ga.Generation = 2
	f.ga.Peers[0].Scopes[0].ResourceGeneration++
	f.a = f.reconnectA(f.ga)
	if _, ok := oldLease.Valid(); ok {
		t.Fatal("changed resource generation retained old lease")
	}
	f.ga.Generation = 3
	f.ga.Peers[0].Scopes[0].ResourceGeneration--
	f.a = f.reconnectA(f.ga)
	if _, ok := oldLease.Valid(); ok {
		t.Fatal("restoring authority revived old lease")
	}
	a.send(t, false, bytes.Repeat([]byte{4}, 1312))
	noUDP(t, b)
}
