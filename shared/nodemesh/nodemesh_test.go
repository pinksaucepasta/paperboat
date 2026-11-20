package nodemesh

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func testCert(t *testing.T, notBefore, notAfter time.Time) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node"}, NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, leaf
}

func validCert(t *testing.T) (tls.Certificate, *x509.Certificate) {
	return testCert(t, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
}

func peerFor(id string, leaf *x509.Certificate) Peer {
	return Peer{NodeID: id, Region: "r1", Host: "198.51.100.1", Port: 443, SPKISHA256: SPKIPin(leaf)}
}

func TestWireEncodingIsPinned(t *testing.T) {
	cases := map[string]struct {
		value any
		want  string
	}{
		"open":  {Open{V: 1, Name: "a.example.test", Visitor: "192.0.2.1:5000", From: "n1", Hop: 1}, `{"v":1,"name":"a.example.test","visitor":"192.0.2.1:5000","from":"n1","hop":1}`},
		"ack":   {OpenAck{Refusal: RefusalNotHolder}, `{"ok":false,"refusal":"not_holder"}`},
		"watch": {Watch{Kind: WatchSnapshot, Names: []string{"a.example.test"}, More: true}, `{"kind":"snapshot","names":["a.example.test"],"more":true}`},
		"peer":  {Peer{NodeID: "n1", Region: "r1", Host: "h", Port: 443, SPKISHA256: "sha256:00"}, `{"node_id":"n1","region":"r1","host":"h","port":443,"spki_sha256":"sha256:00"}`},
	}
	for name, c := range cases {
		got, err := json.Marshal(c.value)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %s want %s (%v)", name, got, c.want, err)
		}
	}
}

func TestOpenValidation(t *testing.T) {
	good := Open{V: 1, Name: "a.example.test", Visitor: "192.0.2.1:5000", From: "n1", Hop: ForwardHop}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Open){
		"hop two":      func(o *Open) { o.Hop = 2 },
		"hop zero":     func(o *Open) { o.Hop = 0 },
		"version":      func(o *Open) { o.V = 2 },
		"upper case":   func(o *Open) { o.Name = "A.example.test" },
		"empty from":   func(o *Open) { o.From = "" },
		"bad visitor":  func(o *Open) { o.Visitor = "not-an-address" },
		"long name":    func(o *Open) { o.Name = strings.Repeat("a", 64) + ".example.test" },
		"trailing dot": func(o *Open) { o.Name = "a.example.test." },
	} {
		o := good
		mutate(&o)
		if err := o.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestWatchBounds(t *testing.T) {
	names := make([]string, MaxNamesPerFrame+1)
	for i := range names {
		names[i] = "a.example.test"
	}
	if err := (Watch{Kind: WatchAdd, Names: names}).Validate(); !errors.Is(err, ErrFrame) {
		t.Fatalf("oversized watch frame accepted: %v", err)
	}
	if err := (Watch{Kind: WatchEnd, Names: names[:1]}).Validate(); err == nil {
		t.Fatal("end frame with names accepted")
	}
	if err := (Watch{Kind: "other"}).Validate(); err == nil {
		t.Fatal("unknown watch kind accepted")
	}
}

func TestFrameBoundsAndTolerantDecode(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, Open{V: 1, Name: "a.example.test", Visitor: "192.0.2.1:1", From: "n", Hop: 1}); err != nil {
		t.Fatal(err)
	}
	var open Open
	if err := ReadMessage(&buf, &open); err != nil || open.Name != "a.example.test" {
		t.Fatalf("round trip: %+v %v", open, err)
	}
	// A field a newer node adds is ignored.
	body := []byte(`{"v":1,"name":"a.example.test","visitor":"192.0.2.1:1","from":"n","hop":1,"later":true}`)
	buf.Reset()
	buf.Write([]byte{0, 0, 0, byte(len(body))})
	buf.Write(body)
	if err := ReadMessage(&buf, &open); err != nil {
		t.Fatalf("unknown field refused: %v", err)
	}
	// Oversized and empty frames are refused before any allocation of the claimed size.
	for _, n := range []uint32{0, MaxFrame + 1} {
		hdr := []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
		if err := ReadMessage(bytes.NewReader(hdr), &open); !errors.Is(err, ErrFrame) {
			t.Errorf("length %d: %v", n, err)
		}
	}
	if err := WriteMessage(&buf, Watch{Kind: WatchAdd, Names: []string{strings.Repeat("a", MaxFrame)}}); !errors.Is(err, ErrFrame) {
		t.Fatalf("oversized write accepted: %v", err)
	}
}

func TestPeerSetRefusesDuplicatesAndBounds(t *testing.T) {
	_, a := validCert(t)
	_, b := validCert(t)
	pa, pb := peerFor("a", a), peerFor("b", b)
	set, err := NewPeerSet([]Peer{pb, pa})
	if err != nil || set.Len() != 2 || set.Peers()[0].NodeID != "a" {
		t.Fatalf("set: %v %v", set.Peers(), err)
	}
	if got, ok := set.ByPin(strings.ToUpper(pa.SPKISHA256)); !ok || got.NodeID != "a" {
		t.Fatalf("pin lookup: %v %v", got, ok)
	}
	if _, err := NewPeerSet([]Peer{pa, pa}); err == nil {
		t.Fatal("duplicate node accepted")
	}
	dup := pb
	dup.SPKISHA256 = pa.SPKISHA256
	if _, err := NewPeerSet([]Peer{pa, dup}); err == nil {
		t.Fatal("duplicate pin accepted")
	}
	if _, err := NewPeerSet(make([]Peer, MaxPeers+1)); err == nil {
		t.Fatal("oversized directory accepted")
	}
}

// handshake connects client to server over loopback TCP and reports both sides' handshake errors.
func handshake(t *testing.T, server, client *tls.Config) (srvErr, cliErr error, state tls.ConnectionState) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		s := tls.Server(c, server)
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		err = s.Handshake()
		if err == nil {
			state = s.ConnectionState()
			buf := make([]byte, 1)
			_, _ = s.Read(buf) // drain the client's close so the client's handshake error surfaces first
		}
		done <- err
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cl := tls.Client(c, client)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	cliErr = cl.Handshake()
	if cliErr == nil {
		_, _ = cl.Write([]byte{1})
	}
	_ = cl.Close()
	srvErr = <-done
	return srvErr, cliErr, state
}

func TestMutualPinHandshake(t *testing.T) {
	serverCert, serverLeaf := validCert(t)
	clientCert, clientLeaf := validCert(t)
	set, err := NewPeerSet([]Peer{peerFor("client", clientLeaf)})
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := ClientTLS(clientCert, EdgeForwardALPN, peerFor("server", serverLeaf))
	if err != nil {
		t.Fatal(err)
	}
	srvErr, cliErr, state := handshake(t, ServerTLS(serverCert, EdgeForwardALPN, func() PeerSet { return set }), clientConfig)
	if srvErr != nil || cliErr != nil {
		t.Fatalf("pinned handshake failed: server %v client %v", srvErr, cliErr)
	}
	if state.NegotiatedProtocol != EdgeForwardALPN {
		t.Fatalf("ALPN %q", state.NegotiatedProtocol)
	}
	if p, ok := PeerOf(state, set); !ok || p.NodeID != "client" {
		t.Fatalf("PeerOf: %v %v", p, ok)
	}
}

func TestPeerPinMismatchRefused(t *testing.T) {
	serverCert, serverLeaf := validCert(t)
	clientCert, clientLeaf := validCert(t)
	_, strangerLeaf := validCert(t)
	known, _ := NewPeerSet([]Peer{peerFor("client", clientLeaf)})
	empty, _ := NewPeerSet(nil)
	serverConfig := func(set PeerSet) *tls.Config {
		return ServerTLS(serverCert, EdgeForwardALPN, func() PeerSet { return set })
	}
	t.Run("server presents a certificate other than the pin", func(t *testing.T) {
		cfg, _ := ClientTLS(clientCert, EdgeForwardALPN, peerFor("server", strangerLeaf))
		if _, cliErr, _ := handshake(t, serverConfig(known), cfg); !errors.Is(cliErr, ErrPinMismatch) {
			t.Fatalf("client accepted a wrong server: %v", cliErr)
		}
	})
	t.Run("client certificate not in the directory", func(t *testing.T) {
		cfg, _ := ClientTLS(clientCert, EdgeForwardALPN, peerFor("server", serverLeaf))
		srvErr, _, _ := handshake(t, serverConfig(empty), cfg)
		if !errors.Is(srvErr, ErrPeerUnknown) {
			t.Fatalf("server accepted an unknown node: %v", srvErr)
		}
	})
	t.Run("expired client certificate", func(t *testing.T) {
		oldCert, oldLeaf := testCert(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
		set, _ := NewPeerSet([]Peer{peerFor("old", oldLeaf)})
		cfg, _ := ClientTLS(oldCert, EdgeForwardALPN, peerFor("server", serverLeaf))
		if srvErr, _, _ := handshake(t, serverConfig(set), cfg); !errors.Is(srvErr, ErrPeerUnknown) {
			t.Fatalf("server accepted an expired certificate: %v", srvErr)
		}
	})
	t.Run("other ALPN", func(t *testing.T) {
		cfg, _ := ClientTLS(clientCert, RelayMeshALPN, peerFor("server", serverLeaf))
		if srvErr, cliErr, _ := handshake(t, serverConfig(known), cfg); srvErr == nil && cliErr == nil {
			t.Fatal("ALPN mismatch accepted")
		}
	})
	t.Run("no client certificate", func(t *testing.T) {
		cfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, NextProtos: []string{EdgeForwardALPN}}
		if srvErr, _, _ := handshake(t, serverConfig(known), cfg); srvErr == nil {
			t.Fatal("server accepted a client without a certificate")
		}
	})
}

func TestHoldersRequestBounds(t *testing.T) {
	ok := HoldersRequest{NodeID: "n", ProcessEpoch: "e"}
	if ok.Validate() != nil || ok.PageLimit() != DefaultHolderPage {
		t.Fatal("default request refused")
	}
	for _, limit := range []int{-1, MaxHolderPage + 1} {
		r := ok
		r.Limit = limit
		if r.Validate() == nil {
			t.Errorf("limit %d accepted", limit)
		}
	}
	if (Holder{Name: "a.example.test"}).Validate() == nil {
		t.Error("holder without nodes accepted")
	}
}

func TestTCPKeys(t *testing.T) {
	if got := TCPKey(10001); got != "tcp:10001" {
		t.Fatalf("TCPKey: %q", got)
	}
	if port, ok := ParseTCPKey("tcp:10001"); !ok || port != 10001 {
		t.Fatalf("ParseTCPKey: %d %v", port, ok)
	}
	for _, bad := range []string{"tcp:", "tcp:0", "tcp:01", "tcp:65536", "tcp:1x", "tcp:-1", "tcp:123456", "TCP:80"} {
		if _, ok := ParseTCPKey(bad); ok {
			t.Errorf("%q parsed", bad)
		}
		if ValidKey(bad) == nil {
			t.Errorf("%q accepted as a key", bad)
		}
	}
	if ValidKey("tcp:10001") != nil || ValidKey("a.example.test") != nil {
		t.Fatal("valid key refused")
	}
	open := Open{V: 1, Name: TCPKey(10001), Visitor: "192.0.2.1:5000", From: "n1", Hop: ForwardHop}
	if open.Validate() != nil || (Watch{Kind: WatchAdd, Names: []string{TCPKey(10001)}}).Validate() != nil || (Holder{Name: TCPKey(10001), NodeIDs: []string{"n1"}}).Validate() != nil {
		t.Fatal("tcp key refused in a message")
	}
}

func TestWildcardKeys(t *testing.T) {
	if got := WildcardKey("a.example.test"); got != "*.a.example.test" {
		t.Fatalf("WildcardKey: %q", got)
	}
	if suffix, ok := WildcardSuffix("*.a.example.test"); !ok || suffix != "a.example.test" {
		t.Fatalf("WildcardSuffix: %q %v", suffix, ok)
	}
	if key, ok := WildcardMatch("x.a.example.test"); !ok || key != "*.a.example.test" {
		t.Fatalf("WildcardMatch: %q %v", key, ok)
	}
	if _, ok := WildcardMatch("localhost"); ok {
		t.Fatal("a one-label name has no wildcard")
	}
	for _, bad := range []string{"*.", "*.*.example.test", "*a.example.test", "a.*.example.test", "*.A.example.test", "**.example.test", "*.example.test."} {
		if ValidKey(bad) == nil {
			t.Errorf("%q accepted as a key", bad)
		}
	}
	for _, good := range []string{"*.a.example.test", "*.example"} {
		if err := ValidKey(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
	if (Holder{Name: "*.a.example.test", NodeIDs: []string{"n1"}}).Validate() != nil {
		t.Fatal("wildcard holder refused")
	}
}

func TestOpenHelloBound(t *testing.T) {
	open := Open{V: 1, Name: "a.example.test", Visitor: "192.0.2.1:5000", From: "n1", Hop: ForwardHop, Hello: make([]byte, MaxHello)}
	if err := open.Validate(); err != nil {
		t.Fatal(err)
	}
	open.Hello = make([]byte, MaxHello+1)
	if err := open.Validate(); !errors.Is(err, ErrFrame) {
		t.Fatalf("oversized hello accepted: %v", err)
	}
	var buf bytes.Buffer
	open.Hello = make([]byte, MaxHello)
	if err := WriteMessage(&buf, open); err != nil {
		t.Fatalf("a full hello must fit one frame: %v", err)
	}
}
