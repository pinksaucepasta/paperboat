package forward_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

const appName = "app.example.test"

// authority is an ingress authority with no decisions: names are served only when the node holds them.
type authority struct{}

func (authority) Snapshot(context.Context) ([]connectorprotocol.IngressDecision, error) {
	return nil, nil
}

func (authority) ResolveDecision(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}

type ca struct {
	cert *x509.Certificate
	key  ed25519.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T, name string) ca {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return ca{cert: cert, key: key, pool: pool}
}

// issue signs a server certificate for names with the CA.
func (c ca) issue(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, c.cert, pub, c.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// linkCertificate is a node's registered carrier certificate: self-signed, identified only by its pin.
func linkCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "edge carrier"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nodemesh.SPKIPin(leaf)
}

// fakeHeld is the set of keys a node serves.
type fakeHeld struct {
	mu  sync.Mutex
	set map[string]bool
}

func (h *fakeHeld) put(keys ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.set == nil {
		h.set = map[string]bool{}
	}
	for _, k := range keys {
		h.set[k] = true
	}
}

func (h *fakeHeld) drop(keys ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range keys {
		delete(h.set, k)
	}
}

func (h *fakeHeld) has(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.set[key]
}

func (h *fakeHeld) Held() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.set))
	for k := range h.set {
		keys = append(keys, k)
	}
	return keys
}

// fakeSource is the server: a peer directory and a holder seed, both set by the test.
type fakeSource struct {
	mu          sync.Mutex
	peers       []nodemesh.Peer
	seed        map[string][]string
	holderCalls atomic.Int64
	failHolders atomic.Bool
}

func (s *fakeSource) setPeers(peers ...nodemesh.Peer) {
	s.mu.Lock()
	s.peers = peers
	s.mu.Unlock()
}

func (s *fakeSource) setSeed(name string, nodes ...string) {
	s.mu.Lock()
	if s.seed == nil {
		s.seed = map[string][]string{}
	}
	s.seed[name] = nodes
	s.mu.Unlock()
}

func (s *fakeSource) Peers(context.Context) ([]nodemesh.Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]nodemesh.Peer(nil), s.peers...), nil
}

func (s *fakeSource) Holders(_ context.Context, page string, limit int) (nodemesh.HoldersResponse, error) {
	s.holderCalls.Add(1)
	if s.failHolders.Load() {
		return nodemesh.HoldersResponse{}, fmt.Errorf("holders must not be asked")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.seed))
	for name := range s.seed {
		if name > page {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var resp nodemesh.HoldersResponse
	for i, name := range names {
		if i == limit {
			resp.Next = names[i-1]
			break
		}
		resp.Holders = append(resp.Holders, nodemesh.Holder{Name: name, NodeIDs: s.seed[name]})
	}
	return resp, nil
}

// node is one edge: a public TLS listener with the production dispatch, an HTTP server behind it, and a mesh.
type node struct {
	t       *testing.T
	id      string
	region  string
	link    tls.Certificate
	pin     string
	port    uint16
	ln      net.Listener
	public  *edgehttp.PublicTCPListeners
	held    *fakeHeld
	src     *fakeSource
	mesh    *forward.Mesh
	hits    atomic.Int64
	drained atomic.Bool
	cfg     func(*forward.Config)
	dialed  []net.Conn
	dialMu  sync.Mutex
}

// echo handler: GET answers who served it; an Upgrade request becomes a raw echo stream.
func (n *node) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.hits.Add(1)
		if r.Header.Get("Upgrade") == "echo" {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
			_ = rw.Flush()
			_, _ = io.Copy(conn, rw)
			return
		}
		fmt.Fprintf(w, "holder=%s host=%s remote=%s", n.id, r.Host, r.RemoteAddr)
	})
}

// newNode starts an edge that answers visitors with visitorCert (the holder's certificate chain) and, for names it
// does not serve, with the page certificate unknownCert.
func newNode(t *testing.T, id string, visitorCert, unknownCert tls.Certificate) *node {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	link, pin := linkCertificate(t)
	n := &node{t: t, id: id, region: "r1", link: link, pin: pin, ln: ln, port: uint16(ln.Addr().(*net.TCPAddr).Port), held: &fakeHeld{}, src: &fakeSource{}}
	registry, err := edgehttp.NewDataCarrierRouteRegistry(edgehttp.DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	n.public, err = edgehttp.NewPublicTCPListeners(edgehttp.PublicTCPListenerConfig{Authority: authority{}, Routes: registry, ListenHost: "127.0.0.1", MaximumListeners: 1, MaximumConnections: 256,
		UnknownNameTLS: &tls.Config{Certificates: []tls.Certificate{unknownCert}, MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := n.public.WrapTLSListener(ln, n.held.has, n.held.has)
	if err != nil {
		t.Fatal(err)
	}
	web := &http.Server{Handler: n.handler(), TLSConfig: &tls.Config{Certificates: []tls.Certificate{visitorCert}, NextProtos: []string{"http/1.1"}}}
	go func() { _ = web.ServeTLS(shared, "", "") }()
	t.Cleanup(func() { _ = web.Close(); _ = shared.Close() })
	return n
}

func (n *node) peer() nodemesh.Peer {
	return nodemesh.Peer{NodeID: n.id, Region: n.region, Host: "127.0.0.1", Port: n.port, SPKISHA256: n.pin}
}

// start builds and starts the node's mesh with fast intervals.
func (n *node) start() {
	n.t.Helper()
	cfg := forward.Config{NodeID: n.id, Region: n.region, Certificate: n.link, Peers: n.src, Holders: n.src, Held: n.held, Injector: n.public,
		DialTimeout: 500 * time.Millisecond, OpenTimeout: time.Second, PeerInterval: 50 * time.Millisecond, SeedInterval: 100 * time.Millisecond,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err == nil {
				n.dialMu.Lock()
				n.dialed = append(n.dialed, conn)
				n.dialMu.Unlock()
			}
			return conn, err
		}}
	if n.cfg != nil {
		n.cfg(&cfg)
	}
	mesh, err := forward.New(cfg)
	if err != nil {
		n.t.Fatal(err)
	}
	n.mesh = mesh
	n.public.SetForwarding(edgehttp.ForwardHooks{Forwarder: mesh, Mesh: mesh})
	ctx, cancel := context.WithCancel(context.Background())
	if err := mesh.Start(ctx); err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() {
		cancel()
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = mesh.Stop(stop)
	})
}

// dropLinks closes every connection this node dialled, as a network cut would.
func (n *node) dropLinks() {
	n.dialMu.Lock()
	defer n.dialMu.Unlock()
	for _, c := range n.dialed {
		_ = c.Close()
	}
	n.dialed = nil
}

// cluster lists every node as every other node's peer and starts the meshes.
func cluster(nodes ...*node) {
	for _, n := range nodes {
		var peers []nodemesh.Peer
		for _, other := range nodes {
			if other != n {
				peers = append(peers, other.peer())
			}
		}
		n.src.setPeers(peers...)
		n.start()
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func holdersOf(n *node, key string) string { return strings.Join(n.mesh.Holders(key), ",") }

// visit sends one HTTPS request to node via, as a visitor who trusts only pool, and returns the response and the
// visitor's own address.
func visit(t *testing.T, via *node, name string, pool *x509.CertPool) (status int, body, local string, err error) {
	t.Helper()
	var localAddr atomic.Value
	transport := &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: name, NextProtos: []string{"http/1.1"}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, via.ln.Addr().String())
			if err == nil {
				localAddr.Store(conn.LocalAddr().String())
			}
			return conn, err
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	resp, err := client.Get("https://" + name + "/")
	address, _ := localAddr.Load().(string)
	if err != nil {
		return 0, "", address, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), address, nil
}

// upgrade opens an HTTP/1.1 Upgrade stream through via and returns the raw TLS connection after the 101.
func upgrade(t *testing.T, via *node, name string, pool *x509.CertPool) (net.Conn, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", via.ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: name, NextProtos: []string{"http/1.1"}})
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", name)
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for !strings.HasSuffix(string(buf), "\r\n\r\n") {
		if _, err := conn.Read(one); err != nil {
			_ = conn.Close()
			return nil, err
		}
		buf = append(buf, one[0])
	}
	if !strings.HasPrefix(string(buf), "HTTP/1.1 101") {
		_ = conn.Close()
		return nil, fmt.Errorf("not upgraded: %q", buf)
	}
	return conn, nil
}

func echoOnce(conn net.Conn, text string) error {
	if _, err := io.WriteString(conn, text); err != nil {
		return err
	}
	got := make([]byte, len(text))
	if _, err := io.ReadFull(conn, got); err != nil {
		return err
	}
	if string(got) != text {
		return fmt.Errorf("echo %q != %q", got, text)
	}
	return nil
}
