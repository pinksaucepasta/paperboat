package derpquic

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

type fixture struct {
	t        *testing.T
	signing  ed25519.PrivateKey
	verifier Verifier
	tls      *tls.Config
	roots    *x509.CertPool
}

func certificate(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: priv, Leaf: leaf}
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := certificate(t)
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	return &fixture{t: t, signing: priv, verifier: Verifier{Issuer: "test-authority", NodeID: "relay", NodeGeneration: 1, ProcessEpoch: "epoch", Keys: map[string]ed25519.PublicKey{"test": pub}}, tls: &tls.Config{Certificates: []tls.Certificate{cert}}, roots: roots}
}
func (f *fixture) token(g Grant) string {
	f.t.Helper()
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"paperboat-relay-grant+jwt","kid":"test"}`))
	b, err := json.Marshal(g)
	if err != nil {
		f.t.Fatal(err)
	}
	body := h + "." + base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.signing, []byte(body)))
}
func (f *fixture) grant(k key.NodePublic, cert tls.Certificate, endpoint string) Grant {
	fp := sha256.Sum256(cert.Certificate[0])
	public := cert.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	now := time.Now().Unix()
	return Grant{Version: 1, Issuer: f.verifier.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: 1, AccountID: "account", EndpointID: endpoint, WireGuardPublicKey: KeyString(k), CertificateFingerprint: hex.EncodeToString(fp[:]), QUICPublicKey: base64.RawURLEncoding.EncodeToString(public), NodeID: f.verifier.NodeID, NodeGeneration: f.verifier.NodeGeneration, ProcessEpoch: f.verifier.ProcessEpoch}
}
func pairScopes(a, b *Grant) {
	scope := Scope{ResourceKind: "machine_access", ResourceID: "machine", ResourceGeneration: 1, Capability: "terminal", Direction: "dial", Port: 443, ExpiresAt: max(a.ExpiresAt, b.ExpiresAt)}
	a.Peers = []Peer{{WireGuardPublicKey: b.WireGuardPublicKey, Scopes: []Scope{scope}}}
	scope.Direction = "accept"
	b.Peers = []Peer{{WireGuardPublicKey: a.WireGuardPublicKey, Scopes: []Scope{scope}}}
}
func (f *fixture) start(address string) (*Server, string, func()) {
	f.t.Helper()
	socket, err := net.ListenPacket("udp4", address)
	if err != nil {
		f.t.Fatal(err)
	}
	server, err := NewServer(f.verifier)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, socket, f.tls) }()
	eventually(f.t, func() bool { server.mu.Lock(); defer server.mu.Unlock(); return server.listener != nil })
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			server.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				f.t.Error("Serve did not stop")
			}
			server.Wait()
			socket.Close()
		})
	}
	f.t.Cleanup(stop)
	return server, socket.LocalAddr().String(), stop
}
func (f *fixture) client(address string, cert tls.Certificate, g Grant) *Client {
	c := NewClient(ClientConfig{Address: address, TLS: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}}, Credential: func(context.Context) (string, error) { return f.token(g), nil }})
	f.t.Cleanup(func() { c.Close() })
	return c
}
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func connect(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
}
func receive(t *testing.T, c *Client, want key.NodePublic, payload []byte) int {
	t.Helper()
	type result struct {
		m   derp.ReceivedMessage
		gen int
		err error
	}
	done := make(chan result, 1)
	go func() { m, g, e := c.RecvDetail(); done <- result{m, g, e} }()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		p, ok := r.m.(derp.ReceivedPacket)
		if !ok || p.Source != want || !bytes.Equal(p.Data, payload) {
			t.Fatal("forwarded source or bytes differ")
		}
		return r.gen
	case <-time.After(3 * time.Second):
		c.Close()
		t.Fatal("receive timed out")
		return 0
	}
}
func TestCarrierForwardRestartRevoke(t *testing.T) {
	f := newFixture(t)
	s, address, stop := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	pairScopes(&ga, &gb)
	a, b := f.client(address, ca, ga), f.client(address, cb, gb)
	connect(t, a)
	connect(t, b)
	data := make([]byte, 1312)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	data[0] = 4
	if err := a.Send(kb, data); err != nil {
		t.Fatal(err)
	}
	first := receive(t, b, ka, data)
	control := []byte("opaque discovery control")
	if err := b.SendControl(ka, control); err != nil {
		t.Fatal(err)
	}
	receive(t, a, kb, control)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Accepted != 2 {
		t.Fatal("missing admission accounting")
	}
	stop()
	eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.current.conn.Context().Err() != nil })
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.current.conn.Context().Err() != nil })
	s, _, _ = f.start(address)
	connect(t, a)
	connect(t, b)
	if err := a.Send(kb, data); err != nil {
		t.Fatal(err)
	}
	if g := receive(t, b, ka, data); g <= first {
		t.Fatal("restart did not advance generation")
	}
	s.Revoke("account", "a", 2)
	eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.current.conn.Context().Err() != nil })
	if err := a.SendControl(kb, control); !errors.Is(err, ErrAdmission) {
		t.Fatalf("revoked client: %v", err)
	}
	if err := a.Connect(context.Background()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("fatal admission was not latched: %v", err)
	}
	stale := f.client(address, ca, ga)
	if err := stale.Connect(context.Background()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("stale reconnect: %v", err)
	}
	ga.Generation = 3
	fresh := f.client(address, ca, ga)
	connect(t, fresh)
	if err := fresh.SendControl(kb, control); err != nil {
		t.Fatal(err)
	}
	receive(t, b, ka, control)
}

func TestRelayControlAuthorityCannotForwardApplicationPackets(t *testing.T) {
	f := newFixture(t)
	server, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "initiator"), f.grant(kb, cb, "machine-relay")
	ga.RelayControlPeers = []string{gb.WireGuardPublicKey}
	gb.RelayControlPeers = []string{ga.WireGuardPublicKey}
	a, b := f.client(address, ca, ga), f.client(address, cb, gb)
	connect(t, a)
	connect(t, b)
	control := []byte("sealed relay allocation")
	if err := a.SendControl(kb, control); err != nil {
		t.Fatal(err)
	}
	receive(t, b, ka, control)
	before := server.Snapshot()
	packet := append([]byte{4}, make([]byte, 127)...)
	if err := a.Send(kb, packet); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if server.Snapshot().Forwarded != before.Forwarded {
		t.Fatal("control-only relationship forwarded an application packet")
	}
}
func TestAdmissionFailures(t *testing.T) {
	f := newFixture(t)
	_, address, _ := f.start("127.0.0.1:0")
	for _, tc := range []struct {
		name string
		edit func(*Grant)
	}{
		{"certificate", func(g *Grant) { g.CertificateFingerprint = "wrong" }},
		{"quic key", func(g *Grant) {
			g.QUICPublicKey = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
		}},
		{"expired", func(g *Grant) {
			g.IssuedAt = time.Now().Add(-time.Minute).Unix()
			g.ExpiresAt = time.Now().Add(-time.Second).Unix()
		}},
		{"node_generation", func(g *Grant) { g.NodeGeneration++ }},
		{"process_epoch", func(g *Grant) { g.ProcessEpoch = "stale" }},
		{"empty_account", func(g *Grant) { g.AccountID = "" }},
		{"expired_invalid_account", func(g *Grant) {
			g.AccountID = ""
			g.IssuedAt = time.Now().Add(-time.Minute).Unix()
			g.ExpiresAt = time.Now().Add(-time.Second).Unix()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert := certificate(t)
			g := f.grant(key.NewNode().Public(), cert, tc.name)
			tc.edit(&g)
			c := f.client(address, cert, g)
			want := ErrAdmission
			if tc.name == "expired" {
				want = ErrExpired
			}
			if err := c.Connect(context.Background()); !errors.Is(err, want) {
				t.Fatalf("wanted %v, got %v", want, err)
			}
		})
	}
}
func TestMismatchedScopeDenied(t *testing.T) {
	for _, mode := range []string{"scope_generation", "scope_direction"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			s, address, _ := f.start("127.0.0.1:0")
			ka, kb := key.NewNode().Public(), key.NewNode().Public()
			ca, cb := certificate(t), certificate(t)
			ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
			pairScopes(&ga, &gb)
			switch mode {
			case "scope_generation":
				gb.Peers[0].Scopes[0].ResourceGeneration++
			case "scope_direction":
				gb.Peers[0].Scopes[0].Direction = "dial"
			}
			a, b := f.client(address, ca, ga), f.client(address, cb, gb)
			connect(t, a)
			connect(t, b)
			if err := a.SendControl(kb, []byte("denied")); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { return s.Snapshot().Dropped > 0 })
			b.mu.Lock()
			cc := b.current
			b.mu.Unlock()
			select {
			case <-cc.receive:
				t.Fatal("unauthorized traffic delivered")
			case <-time.After(30 * time.Millisecond):
			}
			if s.Snapshot().Forwarded != 0 {
				t.Fatal("unauthorized forwarding accounted")
			}
		})
	}
}

func TestCrossAccountExactScopeForwarded(t *testing.T) {
	f := newFixture(t)
	_, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "shared_client"), f.grant(kb, cb, "shared_machine")
	gb.AccountID = "machine_owner"
	pairScopes(&ga, &gb)
	a, b := f.client(address, ca, ga), f.client(address, cb, gb)
	connect(t, a)
	connect(t, b)
	payload := []byte("team machine exact grant")
	if err := a.Send(kb, payload); err != nil {
		t.Fatal(err)
	}
	receive(t, b, ka, payload)
}
func TestCancellationAndPacketBounds(t *testing.T) {
	f := newFixture(t)
	_, address, _ := f.start("127.0.0.1:0")
	cert := certificate(t)
	c := f.client(address, cert, f.grant(key.NewNode().Public(), cert, "a"))
	connect(t, c)
	for _, n := range []int{0, MaxPacket + 1} {
		if err := c.Send(key.NewNode().Public(), make([]byte, n)); !errors.Is(err, ErrProtocol) {
			t.Fatalf("packet size %d: %v", n, err)
		}
	}
	done := make(chan error, 1)
	go func() { _, _, e := c.RecvDetail(); done <- e }()
	c.Close()
	select {
	case e := <-done:
		if !errors.Is(e, ErrClosed) {
			t.Fatalf("receive close: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel receive")
	}
	blocked := NewClient(ClientConfig{Address: address, TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Credential: func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }})
	defer blocked.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := blocked.Connect(ctx); !errors.Is(e, context.Canceled) {
		t.Fatalf("connect cancellation: %v", e)
	}
}

func TestLiveExpiryAndDrain(t *testing.T) {
	for _, mode := range []string{"expiry", "drain"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			s, address, _ := f.start("127.0.0.1:0")
			cert := certificate(t)
			g := f.grant(key.NewNode().Public(), cert, "a")
			if mode == "expiry" {
				// Unix-second grants need admission headroom even near a second boundary.
				g.ExpiresAt = time.Now().Add(2 * time.Second).Unix()
			}
			c := f.client(address, cert, g)
			connect(t, c)
			if mode == "drain" {
				s.Drain(time.Now())
				if !s.Snapshot().Draining {
					t.Fatal("drain health missing")
				}
				otherCert := certificate(t)
				other := f.client(address, otherCert, f.grant(key.NewNode().Public(), otherCert, "b"))
				if err := other.Connect(context.Background()); !errors.Is(err, ErrOverload) {
					t.Fatalf("drain admission: %v", err)
				}
			}
			c.mu.Lock()
			cc := c.current
			c.mu.Unlock()
			select {
			case <-cc.conn.Context().Done():
			case <-time.After(3 * time.Second):
				t.Fatal("expired or drained connection remains live")
			}
			want := ErrExpired
			if mode == "drain" {
				want = ErrOverload
			}
			if err := classify(context.Cause(cc.conn.Context())); !errors.Is(err, want) {
				t.Fatalf("expired/drained connection: want %v, got %v", want, err)
			}
		})
	}
}

func TestSignedTokenTampering(t *testing.T) {
	f := newFixture(t)
	_, address, _ := f.start("127.0.0.1:0")
	cert := certificate(t)
	g := f.grant(key.NewNode().Public(), cert, "a")
	token := f.token(g)
	// Change signed payload while retaining a valid JSON grant and original signature.
	g.AccountID = "tampered"
	payload, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	parts := bytes.Split([]byte(token), []byte("."))
	parts[1] = []byte(base64.RawURLEncoding.EncodeToString(payload))
	token = string(bytes.Join(parts, []byte(".")))
	c := NewClient(ClientConfig{Address: address, TLS: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}}, Credential: func(context.Context) (string, error) { return token, nil }})
	defer c.Close()
	if err := c.Connect(context.Background()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("tampered grant: %v", err)
	}
}

func TestAccountConnectionBoundAndRelease(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	var clients []*Client
	for i := 0; i < MaxAccountConnections; i++ {
		cert := certificate(t)
		c := f.client(address, cert, f.grant(key.NewNode().Public(), cert, string(rune('a'+i))))
		connect(t, c)
		clients = append(clients, c)
	}
	cert := certificate(t)
	g := f.grant(key.NewNode().Public(), cert, "overflow")
	overflow := f.client(address, cert, g)
	if err := overflow.Connect(context.Background()); !errors.Is(err, ErrOverload) {
		t.Fatalf("account overflow: %v", err)
	}
	clients[0].Close()
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.peers) == MaxAccountConnections-1 })
	connect(t, overflow)
	if stats := s.Snapshot(); stats.Accepted != MaxAccountConnections+1 || stats.Denied == 0 {
		t.Fatalf("connection accounting: %+v", stats)
	}
}

func TestReconnectSupersedesLiveConnection(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	cert := certificate(t)
	g := f.grant(key.NewNode().Public(), cert, "a")
	old := f.client(address, cert, g)
	connect(t, old)
	old.mu.Lock()
	previous := old.current
	old.mu.Unlock()
	replacement := f.client(address, cert, g)
	connect(t, replacement)
	select {
	case <-previous.conn.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("old connection was not superseded")
	}
	s.mu.Lock()
	live := s.peers
	count := len(live)
	s.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected one current registration, got %d", count)
	}
	if err := old.SendControl(key.NewNode().Public(), []byte("reconnected")); err != nil {
		t.Fatalf("same-authority reconnect: %v", err)
	}
	old.mu.Lock()
	reconnected := old.current
	old.mu.Unlock()
	if reconnected == nil || reconnected == previous {
		t.Fatal("superseded client did not establish a fresh physical connection")
	}
}

// Expiry withdraws forwarding until fresh signed authority arrives, but must not
// permanently fence the carrier like an explicit revocation does.
func TestExpiredLeaseRecoversWithFreshAuthority(t *testing.T) { testExpiredLeaseRecovers(t, false) }
func TestFallbackExpiredLeaseRecoversWithFreshAuthority(t *testing.T) {
	testExpiredLeaseRecovers(t, true)
}
func testExpiredLeaseRecovers(t *testing.T, fallback bool) {
	f := newFixture(t)
	server, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	pairScopes(&ga, &gb)
	ga.ExpiresAt = time.Now().Add(2 * time.Second).Unix()
	var mu sync.Mutex
	token := f.token(ga)
	a := NewClient(ClientConfig{Address: address, TLS: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{ca}}, Credential: func(context.Context) (string, error) { mu.Lock(); defer mu.Unlock(); return token, nil }})
	t.Cleanup(func() { a.Close() })
	var carrier Carrier = a
	if fallback {
		wss, _ := f.wss(server, ca, ga, nil)
		wss.config.Credential = a.config.Credential
		combined := NewFallbackCarrier(a, wss, time.Second)
		t.Cleanup(func() { combined.Close() })
		carrier = combined
	}
	b := f.client(address, cb, gb)
	if err := carrier.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	connect(t, b)
	a.mu.Lock()
	old := a.current
	a.mu.Unlock()
	select {
	case <-old.conn.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("expired lease remained connected")
	}
	before := server.Snapshot().Forwarded
	// Re-presenting expired signed authority remains denied and forwards nothing.
	if err := carrier.Send(kb, []byte("expired")); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired grant must report recoverable expiry: %v", err)
	}
	if server.Snapshot().Forwarded != before {
		t.Fatal("expired grant forwarded traffic")
	}
	ga.Generation++
	ga.IssuedAt = time.Now().Unix()
	ga.ExpiresAt = ga.IssuedAt + 30
	mu.Lock()
	token = f.token(ga)
	mu.Unlock()

	payload := []byte("renewed")
	if err := carrier.Send(kb, payload); err != nil {
		t.Fatal(err)
	}
	receive(t, b, ka, payload)
}
