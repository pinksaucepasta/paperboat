package edgehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// blockingListener is a listener that binds nothing: a node whose real sockets are not the point of a test.
type blockingListener struct {
	addr net.Addr
	done chan struct{}
	once sync.Once
}

func newBlockingListener(address string) *blockingListener {
	host, port, _ := net.SplitHostPort(address)
	p, _ := strconv.Atoi(port)
	return &blockingListener{addr: &net.TCPAddr{IP: net.ParseIP(host), Port: p}, done: make(chan struct{})}
}
func (l *blockingListener) Accept() (net.Conn, error) { <-l.done; return nil, net.ErrClosed }
func (l *blockingListener) Close() error              { l.once.Do(func() { close(l.done) }); return nil }
func (l *blockingListener) Addr() net.Addr            { return l.addr }

// tcpNode is one edge's public listeners with its own authority and carrier registry.
type tcpNode struct {
	p         *PublicTCPListeners
	authority *publicTCPAuthorityFixture
	routes    *DataCarrierRouteRegistry
	binds     atomic.Int64
}

// newTCPNode builds an edge; real controls whether it binds real sockets.
func newTCPNode(t *testing.T, real bool, allowed func(host, address string) bool) *tcpNode {
	t.Helper()
	n := &tcpNode{authority: &publicTCPAuthorityFixture{}}
	var err error
	if n.routes, err = NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{AddressAllowed: allowed}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.routes.Close() })
	cfg := PublicTCPListenerConfig{ListenHost: "127.0.0.1", Authority: n.authority, Routes: n.routes, Interval: time.Hour}
	if !real {
		cfg.Listen = func(_, address string) (net.Listener, error) {
			n.binds.Add(1)
			return newBlockingListener(address), nil
		}
	}
	if n.p, err = NewPublicTCPListeners(cfg); err != nil {
		t.Fatal(err)
	}
	if err = n.p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.p.Shutdown(context.Background()) })
	return n
}

func (n *tcpNode) listening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
	if err == nil {
		_ = c.Close()
	}
	return err == nil
}

func (n *tcpNode) listenerCount() int {
	n.p.mu.Lock()
	defer n.p.mu.Unlock()
	return len(n.p.listeners)
}

// visitorConn is a link stream: its RemoteAddr is the visitor the forwarding node saw.
type visitorConn struct {
	*net.TCPConn
	remote net.Addr
}

func (c *visitorConn) RemoteAddr() net.Addr { return c.remote }

// directory is a forwarder over in-process holders: the node link of the real mesh without its TLS.
type directory struct {
	t       *testing.T
	mu      sync.Mutex
	ports   []uint16
	holders map[string][]*tcpNode
	opened  atomic.Int64
}

func (d *directory) setPorts(ports ...uint16) { d.mu.Lock(); d.ports = ports; d.mu.Unlock() }
func (d *directory) TCPPorts() []uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]uint16(nil), d.ports...)
}
func (d *directory) Holders(key string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, len(d.holders[key]))
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}
	return ids
}

func (d *directory) Forward(_ context.Context, conn net.Conn, key string) error {
	d.mu.Lock()
	holders := d.holders[key]
	d.mu.Unlock()
	for _, h := range holders {
		dialed, accepted := tcpPair(d.t)
		visitor := &visitorConn{TCPConn: accepted, remote: conn.RemoteAddr()}
		var err error
		if port, ok := nodemesh.ParseTCPKey(key); ok {
			err = h.p.InjectTCP(visitor, port)
		} else {
			err = h.p.Inject(visitor, conn.RemoteAddr())
		}
		if err != nil {
			_ = dialed.Close()
			_ = accepted.Close()
			continue
		}
		d.opened.Add(1)
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = io.Copy(dialed, conn); _ = dialed.CloseWrite() })
		wg.Go(func() {
			_, _ = io.Copy(conn, dialed)
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		})
		wg.Wait()
		return nil
	}
	return errors.New("no holder took the visitor")
}

// tcpHolder attaches a carrier for the decision's tunnel and returns the connector side and the decision.
func tcpHolder(t *testing.T, n *tcpNode, port uint16) (*datacarrier.Client, connectorprotocol.IngressDecision) {
	t.Helper()
	carrier, connector := testEdgePreviewCarrierPair(t, replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3))
	if err := n.routes.Attach(carrier); err != nil {
		t.Fatal(err)
	}
	decision := publicTCPDecision(time.Now().UTC(), port)
	n.authority.set(decision)
	return connector, decision
}

func freePublicPort(t *testing.T) uint16 {
	t.Helper()
	probe, err := listenInPublicTCPRange()
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	return uint16(probe.Addr().(*net.TCPAddr).Port)
}

func readStreamOpen(t *testing.T, connector *datacarrier.Client) (*datacarrier.Stream, datacarrier.StreamOpen) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, open, err := connector.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connectorprotocol.ReadIngressDecision(stream, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return stream, open
}

func TestForwardTCPFromNonHolder(t *testing.T) {
	port := freePublicPort(t)
	holder := newTCPNode(t, false, nil)
	connector, _ := tcpHolder(t, holder, port)
	forwarder := newTCPNode(t, true, nil)
	dir := &directory{t: t, ports: []uint16{port}, holders: map[string][]*tcpNode{nodemesh.TCPKey(port): {holder}}}
	forwarder.p.SetForwarding(ForwardHooks{Forwarder: dir})
	if err := forwarder.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The forwarder has no carrier and no decision: it listens only because the directory names the port.
	visitor, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		t.Fatalf("a non-holder does not listen on a tunnel port its peers hold: %v", err)
	}
	defer visitor.Close()
	if _, err := visitor.Write([]byte("select 1;")); err != nil {
		t.Fatal(err)
	}
	stream, open := readStreamOpen(t, connector)
	if open.Kind != "tcp_public" {
		t.Fatalf("open = %+v", open)
	}
	got := make([]byte, 9)
	if _, err := io.ReadFull(stream, got); err != nil || string(got) != "select 1;" {
		t.Fatalf("bytes at the holder's carrier: %q %v", got, err)
	}
	if _, err := stream.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(visitor, reply); err != nil || string(reply) != "ok" {
		t.Fatalf("reply to the visitor: %q %v", reply, err)
	}
}

func TestForwardTCPPreservesVisitorAddressForIPRules(t *testing.T) {
	port := freePublicPort(t)
	// The owner denies 127.0.0.2; the forwarding node itself connects from 127.0.0.1.
	holder := newTCPNode(t, false, func(_, address string) bool { return address != "127.0.0.2" })
	connector, _ := tcpHolder(t, holder, port)
	forwarder := newTCPNode(t, true, nil)
	forwarder.p.SetForwarding(ForwardHooks{Forwarder: &directory{t: t, ports: []uint16{port}, holders: map[string][]*tcpNode{nodemesh.TCPKey(port): {holder}}}})
	if err := forwarder.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	denied, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}).Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Close()
	_, _ = denied.Write([]byte("x"))
	_ = denied.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := denied.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("the owner's deny list did not block the real visitor: %d %v", n, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := connector.AcceptStream(ctx); err == nil {
		t.Fatal("a denied visitor reached the carrier")
	}
	allowed, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer allowed.Close()
	_, _ = allowed.Write([]byte("y"))
	stream, _ := readStreamOpen(t, connector)
	got := make([]byte, 1)
	if _, err := io.ReadFull(stream, got); err != nil || got[0] != 'y' {
		t.Fatalf("allowed visitor: %q %v", got, err)
	}
	t.Run("allowlist", forwardTCPAllowlistCase)
}

// forwardTCPAllowlistCase (a part of TestForwardTCPPreservesVisitorAddressForIPRules): the owner's allowlist is evaluated on the attested visitor, exactly as for a direct one: the forwarding node's own
// address (127.0.0.1) does not satisfy it, the visitor's (127.0.0.2) does.
func forwardTCPAllowlistCase(t *testing.T) {
	port := freePublicPort(t)
	holder := newTCPNode(t, false, func(_, address string) bool { return address == "127.0.0.2" })
	connector, _ := tcpHolder(t, holder, port)
	forwarder := newTCPNode(t, true, nil)
	forwarder.p.SetForwarding(ForwardHooks{Forwarder: &directory{t: t, ports: []uint16{port}, holders: map[string][]*tcpNode{nodemesh.TCPKey(port): {holder}}}})
	if err := forwarder.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	outside, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	_, _ = outside.Write([]byte("x"))
	_ = outside.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := outside.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("a visitor outside the allowlist got through: %d %v", n, err)
	}
	inside, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}).Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer inside.Close()
	_, _ = inside.Write([]byte("y"))
	stream, _ := readStreamOpen(t, connector)
	got := make([]byte, 1)
	if _, err := io.ReadFull(stream, got); err != nil || got[0] != 'y' {
		t.Fatalf("allowlisted visitor: %q %v", got, err)
	}
}

func TestForwardTCPMeteredOnHolderOnly(t *testing.T) {
	port := freePublicPort(t)
	holder := newTCPNode(t, false, nil)
	connector, _ := tcpHolder(t, holder, port)
	forwarder := newTCPNode(t, true, nil)
	forwarder.p.SetForwarding(ForwardHooks{Forwarder: &directory{t: t, ports: []uint16{port}, holders: map[string][]*tcpNode{nodemesh.TCPKey(port): {holder}}}})
	if err := forwarder.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	upload, reply := bytes.Repeat([]byte("u"), 40000), bytes.Repeat([]byte("r"), 30000)
	inBefore, outBefore := edgetelemetry.TrafficTotals()
	visitor, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		t.Fatal(err)
	}
	defer visitor.Close()
	go func() { _, _ = visitor.Write(upload); _ = visitor.(*net.TCPConn).CloseWrite() }()
	stream, _ := readStreamOpen(t, connector)
	got, err := io.ReadAll(stream)
	if err != nil || len(got) != len(upload) {
		t.Fatalf("upload at the carrier: %d %v", len(got), err)
	}
	_, _ = stream.Write(reply)
	_ = stream.Close()
	if back, err := io.ReadAll(visitor); err != nil || len(back) != len(reply) {
		t.Fatalf("reply at the visitor: %d %v", len(back), err)
	}
	time.Sleep(200 * time.Millisecond) // the holder's bridge records its totals as it ends
	in, out := edgetelemetry.TrafficTotals()
	// Both nodes run in this process and share its counters: metered on the holder only, the totals move by the
	// payload once; metered on both, by twice that.
	if din, dout := in-inBefore, out-outBefore; din < uint64(len(upload)) || din >= 2*uint64(len(upload)) || dout < uint64(len(reply)) || dout >= 2*uint64(len(reply)) {
		t.Fatalf("totals moved ingress +%d (upload %d) egress +%d (reply %d): forwarder metered too", din, len(upload), dout, len(reply))
	}
}

func TestForwardTCPNotHolderTriesNextHolder(t *testing.T) {
	port := freePublicPort(t)
	stale := newTCPNode(t, false, nil) // listed as a holder, no decision and no carrier any more
	holder := newTCPNode(t, false, nil)
	connector, _ := tcpHolder(t, holder, port)
	// A node with a decision but no attached carrier also refuses before the visitor is acknowledged.
	noCarrier := newTCPNode(t, false, nil)
	noCarrier.authority.set(publicTCPDecision(time.Now().UTC(), port))
	if err := stale.p.InjectTCP(nil, port); err == nil || !errors.Is(err, errNoPublicTCPTunnel) {
		t.Fatalf("a node without the tunnel took the visitor: %v", err)
	}
	if err := noCarrier.p.InjectTCP(nil, port); !errors.Is(err, errNoPublicTCPTunnel) {
		t.Fatalf("a node without the carrier took the visitor: %v", err)
	}
	forwarder := newTCPNode(t, true, nil)
	dir := &directory{t: t, ports: []uint16{port}, holders: map[string][]*tcpNode{nodemesh.TCPKey(port): {stale, noCarrier, holder}}}
	forwarder.p.SetForwarding(ForwardHooks{Forwarder: dir})
	if err := forwarder.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	visitor, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		t.Fatal(err)
	}
	defer visitor.Close()
	_, _ = visitor.Write([]byte("z"))
	stream, _ := readStreamOpen(t, connector)
	got := make([]byte, 1)
	if _, err := io.ReadFull(stream, got); err != nil || got[0] != 'z' || dir.opened.Load() != 1 {
		t.Fatalf("third holder: %q %v opened %d", got, err, dir.opened.Load())
	}
}

func TestPublicTCPListenersBindDirectoryPorts(t *testing.T) {
	p1, p2, p3 := freePublicPort(t), freePublicPort(t), freePublicPort(t)
	n := newTCPNode(t, true, nil)
	dir := &directory{t: t, ports: []uint16{p1, p2}}
	n.p.SetForwarding(ForwardHooks{Forwarder: dir})
	if err := n.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !n.listening(int(p1)) || !n.listening(int(p2)) || n.listening(int(p3)) {
		t.Fatalf("bound ports: p1=%v p2=%v p3=%v", n.listening(int(p1)), n.listening(int(p2)), n.listening(int(p3)))
	}
	// A port whose key disappears from the directory closes.
	dir.setPorts(p1)
	if err := n.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !n.listening(int(p1)) || n.listening(int(p2)) {
		t.Fatalf("after p2 left the directory: p1=%v p2=%v", n.listening(int(p1)), n.listening(int(p2)))
	}
	// A local decision for a port wins: the same socket serves it as the holder, and the directory does not bind it twice.
	n.authority.set(publicTCPListenerDecision(int(p1), "listener_1"), publicTCPListenerDecision(int(p3), "listener_3"))
	dir.setPorts(p1, p3)
	if err := n.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	n.p.mu.Lock()
	one, three := n.p.listeners["listener_1"], n.p.listeners[nodemesh.TCPKey(p1)]
	_, forwardedThree := n.p.listeners[nodemesh.TCPKey(p3)]
	count := len(n.p.listeners)
	n.p.mu.Unlock()
	if one == nil || one.forwardPort != 0 || three != nil || forwardedThree || count != 2 {
		t.Fatalf("local decisions did not win: listener_1=%v forwarded p1=%v forwarded p3=%v listeners=%d", one, three, forwardedThree, count)
	}
	if !n.listening(int(p1)) || !n.listening(int(p3)) {
		t.Fatal("ports of local decisions are not served")
	}
	// When the local decisions go away the directory still names the ports: they stay served, now by forwarding.
	n.authority.set()
	dir.setPorts(p1, p3)
	if err := n.p.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n.listenerCount() != 2 || !n.listening(int(p1)) {
		t.Fatalf("after the decisions went away the directory ports must remain bound: %d listeners", n.listenerCount())
	}
}

func TestPublicTCPListenersBindInBatches(t *testing.T) {
	n := newTCPNode(t, false, nil)
	total := 2*forwardBindBatch + 1
	ports := make([]uint16, 0, total)
	for port := edgeports.PublicTCPMin; len(ports) < total; port++ {
		ports = append(ports, uint16(port))
	}
	n.p.SetForwarding(ForwardHooks{Forwarder: &directory{t: t, ports: ports}})
	for tick, want := range []int64{forwardBindBatch, 2 * forwardBindBatch, int64(total)} {
		if err := n.p.reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := n.binds.Load(); got != want {
			t.Fatalf("after reconcile %d bound %d ports, want %d", tick+1, got, want)
		}
	}
	if err := n.p.reconcile(t.Context()); err != nil || n.binds.Load() != int64(total) || n.listenerCount() != total {
		t.Fatalf("a settled directory must not rebind: %d binds, %d listeners, %v", n.binds.Load(), n.listenerCount(), err)
	}
}

func TestForwardTLSPassthroughFromNonHolder(t *testing.T) {
	const host = "db.customer.test"
	holder := newTCPNode(t, false, nil)
	connector, _ := tcpHolder(t, holder, 443)
	d := publicTCPDecision(time.Now().UTC(), 443)
	d.Binding.Protocol, d.Binding.ListenerID, d.Binding.Hostname = "tls", "listener_tls_443", host
	holder.authority.set(d)
	// The holder's shared TLS listener (no sockets bound; forwarded visitors are injected).
	holderTLS, err := holder.p.WrapTLSListener(newBlockingListener("127.0.0.1:443"), func(string) bool { return false }, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holderTLS.Close() })
	forwarder := newTCPNode(t, false, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forwarderTLS, err := forwarder.p.WrapTLSListener(ln, func(string) bool { return false }, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forwarderTLS.Close() })
	dir := &directory{t: t, holders: map[string][]*tcpNode{host: {holder}}}
	forwarder.p.SetForwarding(ForwardHooks{Forwarder: dir})

	// The visitor's TLS handshake starts at the forwarder: its ClientHello must reach the holder's carrier untouched.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		client := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
		_ = client.Handshake()
	}()
	stream, open := readStreamOpen(t, connector)
	if open.Kind != "tcp_public" {
		t.Fatalf("open = %+v", open)
	}
	hello := make([]byte, 5)
	if _, err := io.ReadFull(stream, hello); err != nil || hello[0] != 0x16 || hello[1] != 0x03 {
		t.Fatalf("not a TLS handshake record at the holder's carrier: %x %v", hello, err)
	}
	rest := make([]byte, 600)
	n, _ := io.ReadAtLeast(stream, rest, 1)
	if !bytes.Contains(rest[:n], []byte(host)) {
		t.Fatalf("the ClientHello with its server name did not reach the holder verbatim")
	}
	if dir.opened.Load() != 1 {
		t.Fatalf("forwarded %d visitors", dir.opened.Load())
	}
}
