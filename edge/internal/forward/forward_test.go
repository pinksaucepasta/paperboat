package forward_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// trio: two CAs, one that signs the holders' visitor certificate and one that signs the page certificate every
// node answers unknown names with. A visitor who trusts only the holder CA therefore verifies a chain only a
// holder has.
type trio struct {
	holder, page ca
}

// stale keeps the nodes' watch deltas from ever being sent (the first snapshot at link-up still is), so a test can
// move a name and watch the forwarders act on maps that have not caught up.
func stale(nodes ...*node) {
	for _, n := range nodes {
		n.cfg = func(cfg *forward.Config) { cfg.HeldInterval = time.Hour }
	}
}

func newTrio(t *testing.T) trio { return trio{holder: newCA(t, "holder"), page: newCA(t, "page")} }

func (c trio) node(t *testing.T, id string) *node {
	return newNode(t, id, c.holder.issue(t, appName, "app2.example.test", "*.example.test"), c.page.issue(t, "*.example.test"))
}

func TestForwardSuccess(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	a.held.put(appName)
	b.held.put("app2.example.test")
	cluster(a, b)
	eventually(t, "b to learn a's names", func() bool { return holdersOf(b, appName) == "edge-a" })

	// The visitor reaches b, which does not hold the name. Its TLS client verifies a certificate chain only the
	// holder has: b forwarded raw bytes and never terminated TLS.
	status, body, local, err := visit(t, b, appName, c.holder.pool)
	if err != nil || status != 200 {
		t.Fatalf("visit through b: status %d err %v", status, err)
	}
	if !strings.Contains(body, "holder=edge-a") || !strings.Contains(body, "host="+appName) || !strings.Contains(body, "remote="+local) {
		t.Fatalf("holder did not serve the visitor at its own address %s: %q", local, body)
	}
	if a.hits.Load() != 1 || b.hits.Load() != 0 {
		t.Fatalf("hits a=%d b=%d", a.hits.Load(), b.hits.Load())
	}
	// b's own certificate is the page certificate: it cannot have terminated the visitor's TLS.
	if _, _, _, err := visit(t, b, "nobody.example.test", c.holder.pool); err == nil {
		t.Fatal("b presented a certificate the holder CA signed")
	}
	if sb, sa := b.mesh.Stats(), a.mesh.Stats(); sb.Forwarded != 1 || sa.Accepted != 1 || sb.BytesOut == 0 || sb.BytesIn == 0 {
		t.Fatalf("stats b=%+v a=%+v", sb, sa)
	}

	// A browser WebSocket upgrade over a forwarded connection is raw bytes: the holder serves it.
	conn, err := upgrade(t, b, appName, c.holder.pool)
	if err != nil {
		t.Fatalf("upgrade through b: %v", err)
	}
	defer conn.Close()
	if err := echoOnce(conn, "hello over the forwarder"); err != nil {
		t.Fatal(err)
	}

	// And the other way: a visitor at a for a name b holds.
	if status, body, _, err = visit(t, a, "app2.example.test", c.holder.pool); err != nil || status != 200 || !strings.Contains(body, "holder=edge-b") {
		t.Fatalf("visit through a: %d %q %v", status, body, err)
	}
}

func TestForwardHolderMovedMidRequest(t *testing.T) {
	c := newTrio(t)
	h1, h2, f := c.node(t, "edge-a"), c.node(t, "edge-b"), c.node(t, "edge-c")
	h1.held.put(appName)
	h2.held.put(appName)
	stale(h1, h2, f)
	cluster(h1, h2, f)
	eventually(t, "f to list both holders", func() bool { return holdersOf(f, appName) == "edge-a,edge-b" })

	// The name leaves h1 while f's map still lists it: h1 answers not_holder and f moves on to h2.
	h1.held.drop(appName)
	time.Sleep(20 * time.Millisecond)
	status, body, _, err := visit(t, f, appName, c.holder.pool)
	if err != nil || status != 200 || !strings.Contains(body, "holder=edge-b") {
		t.Fatalf("visit after the move: %d %q %v", status, body, err)
	}
	if f.mesh.Stats().HolderMiss == 0 {
		t.Fatal("f did not see h1 refuse")
	}
	eventually(t, "f's map to drop h1", func() bool { return holdersOf(f, appName) == "edge-b" })
}

func TestForwardHolderDead(t *testing.T) {
	c := newTrio(t)
	dead, live, f := c.node(t, "edge-a"), c.node(t, "edge-b"), c.node(t, "edge-c")
	live.held.put(appName)
	// dead is listed as a holder but nothing listens at its address any more.
	f.cfg = func(cfg *forward.Config) {
		cfg.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			if strings.HasSuffix(address, ":1") {
				<-ctx.Done() // a blackhole
				return nil, ctx.Err()
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
	}
	deadPeer := dead.peer()
	deadPeer.Port = 1
	f.src.setPeers(deadPeer, live.peer())
	f.src.setSeed(appName, "edge-a", "edge-b")
	live.src.setPeers(f.peer())
	f.start()
	live.start()
	eventually(t, "f to list the dead holder first", func() bool { return holdersOf(f, appName) == "edge-a,edge-b" })

	// The dead holder times out in 500 ms; the next holder serves.
	started := time.Now()
	status, body, _, err := visit(t, f, appName, c.holder.pool)
	if err != nil || status != 200 || !strings.Contains(body, "holder=edge-b") || time.Since(started) > 5*time.Second {
		t.Fatalf("visit with a dead first holder: %d %q %v after %v", status, body, err, time.Since(started))
	}

	// With no live holder the visitor gets the branded 503 within the bounds, not a hang.
	f.src.setPeers(deadPeer)
	eventually(t, "f to forget the live holder", func() bool { return holdersOf(f, appName) == "edge-a" })
	started = time.Now()
	status, body, _, err = visit(t, f, appName, c.page.pool)
	if err != nil || status != 503 || !strings.Contains(body, "Paperboat could not reach the node serving this address") || time.Since(started) > 10*time.Second {
		t.Fatalf("visit with no live holder: %d %q %v after %v", status, body, err, time.Since(started))
	}
	if f.mesh.Stats().Unreachable == 0 {
		t.Fatal("unreachable holders were not counted")
	}
}

func TestForwardLoopPrevention(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	a.held.put(appName)
	b.held.put(appName)
	stale(a, b)
	cluster(a, b)
	eventually(t, "a to list b", func() bool { return holdersOf(a, appName) == "edge-b" })
	eventually(t, "b to list a", func() bool { return holdersOf(b, appName) == "edge-a" })

	// The name leaves both nodes at once; each one's map still lists the other. The visitor at a is forwarded to b
	// once; b refuses (not_holder) and never forwards it back.
	a.held.drop(appName)
	b.held.drop(appName)
	time.Sleep(20 * time.Millisecond)
	status, body, _, err := visit(t, a, appName, c.page.pool)
	if err != nil || status != 503 {
		t.Fatalf("visit at a: %d %q %v", status, body, err)
	}
	if sa, sb := a.mesh.Stats(), b.mesh.Stats(); sa.HolderMiss != 1 || sa.Forwarded != 0 || sb.Forwarded != 0 || sb.HolderMiss != 0 || sb.Unreachable != 0 {
		t.Fatalf("exactly one stream a->b expected: a=%+v b=%+v", sa, sb)
	}

	// A visitor injected on b is never forwarded again, even though b's map still lists a and a holds the name
	// again: b does not hold it, so the visitor gets b's not-found page (forwarded to a it would get a 200).
	a.held.put(appName)
	if holdersOf(b, appName) != "edge-a" {
		t.Fatalf("b's map should still list a: %q", holdersOf(b, appName))
	}
	server, client := tcpPair(t)
	defer client.Close()
	before := b.mesh.Stats()
	if err := b.public.Inject(server, client.RemoteAddr()); err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(client, &tls.Config{RootCAs: c.page.pool, ServerName: appName, NextProtos: []string{"http/1.1"}})
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: " + appName + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 64)
	n, _ := conn.Read(reply)
	if !strings.HasPrefix(string(reply[:n]), "HTTP/1.1 404") {
		t.Fatalf("injected visitor for a name b does not hold: %q", reply[:n])
	}
	if after := b.mesh.Stats(); after.Forwarded != before.Forwarded || after.HolderMiss != before.HolderMiss || after.Unreachable != before.Unreachable {
		t.Fatalf("b forwarded an injected visitor: %+v -> %+v", before, after)
	}
	if a.hits.Load() != 0 {
		t.Fatal("the injected visitor reached a")
	}
}

// A holder refuses an Open whose hop is not 1 (a forwarder that forwards a forwarded visitor), and an Open that
// names another node than the one authenticated on the link.
func TestForwardRefusesHopAndSpoofedOrigin(t *testing.T) {
	c := newTrio(t)
	holder, raw := c.node(t, "edge-b"), c.node(t, "edge-a")
	holder.held.put(appName)
	holder.src.setPeers(raw.peer())
	holder.start() // raw has no mesh: the test speaks the link itself, as raw's certificate

	open := func(o nodemesh.Open) string {
		conf, err := nodemesh.ClientTLS(raw.link, nodemesh.EdgeForwardALPN, holder.peer())
		if err != nil {
			t.Fatal(err)
		}
		tc, err := tls.Dial("tcp", holder.ln.Addr().String(), conf)
		if err != nil {
			t.Fatal(err)
		}
		sess, err := yamux.Client(tc, yamux.DefaultConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		st, err := sess.OpenStream(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = st.Write([]byte{'F'})
		if err := nodemesh.WriteMessage(st, o); err != nil {
			t.Fatal(err)
		}
		var ack nodemesh.OpenAck
		if err := nodemesh.ReadMessage(st, &ack); err != nil {
			t.Fatal(err)
		}
		return ack.Refusal
	}
	good := nodemesh.Open{V: 1, Name: appName, Visitor: "192.0.2.7:4000", From: "edge-a", Hop: 1}
	if got := open(nodemesh.Open{V: 1, Name: appName, Visitor: good.Visitor, From: "edge-a", Hop: 2}); got != nodemesh.RefusalLoop {
		t.Fatalf("hop 2 refusal = %q", got)
	}
	if got := open(nodemesh.Open{V: 1, Name: appName, Visitor: good.Visitor, From: "edge-z", Hop: 1}); got != nodemesh.RefusalBadRequest {
		t.Fatalf("spoofed origin refusal = %q", got)
	}
	if got := open(nodemesh.Open{V: 1, Name: "other.example.test", Visitor: good.Visitor, From: "edge-a", Hop: 1}); got != nodemesh.RefusalNotHolder {
		t.Fatalf("unheld name refusal = %q", got)
	}
	if holder.mesh.Stats().LoopRefused != 1 {
		t.Fatalf("loop refusals = %d", holder.mesh.Stats().LoopRefused)
	}
}

func TestForwardNoPeersDirectoryServesLocalOnly(t *testing.T) {
	c := newTrio(t)
	a := c.node(t, "edge-a")
	a.held.put(appName)
	a.src.failHolders.Store(true) // an empty directory never asks the server for holders
	a.start()
	if status, body, _, err := visit(t, a, appName, c.holder.pool); err != nil || status != 200 || !strings.Contains(body, "holder=edge-a") {
		t.Fatalf("local visitor: %d %q %v", status, body, err)
	}
	if status, _, _, err := visit(t, a, "nobody.example.test", c.page.pool); err != nil || status != 404 {
		t.Fatalf("unknown name: %d %v", status, err)
	}
	time.Sleep(300 * time.Millisecond)
	if calls := a.src.holderCalls.Load(); calls != 0 {
		t.Fatalf("holders asked %d times with no peers", calls)
	}
	if s := a.mesh.Stats(); s.Links != 0 || s.Names != 0 || s.Forwarded != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestPeerPinMismatchAndUnknownNodeRefused(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	b.held.put(appName)
	// a lists b with another node's pin; b does not list a at all.
	wrong := b.peer()
	wrong.SPKISHA256 = a.pin
	a.src.setPeers(wrong)
	a.src.setSeed(appName, "edge-b")
	b.src.setPeers()
	a.start()
	b.start()
	status, _, _, err := visit(t, a, appName, c.page.pool)
	if err != nil || status != 503 {
		t.Fatalf("visit with a mismatched pin: %d %v", status, err)
	}
	time.Sleep(300 * time.Millisecond)
	if a.mesh.Stats().Links != 0 || b.mesh.Stats().Links != 0 || b.mesh.Stats().Accepted != 0 {
		t.Fatalf("a link formed with a mismatched pin: a=%+v b=%+v", a.mesh.Stats(), b.mesh.Stats())
	}

	// With the right pin but b not knowing a: b's server side refuses a's certificate.
	a.src.setPeers(b.peer())
	if status, _, _, err = visit(t, a, appName, c.page.pool); err != nil || status != 503 {
		t.Fatalf("visit to a node that does not know us: %d %v", status, err)
	}
	if b.mesh.Stats().Links != 0 {
		t.Fatalf("b accepted a link from a node outside its directory")
	}
}

func TestForwardDrainAware(t *testing.T) {
	c := newTrio(t)
	draining, live, f := c.node(t, "edge-a"), c.node(t, "edge-b"), c.node(t, "edge-c")
	draining.held.put(appName)
	live.held.put(appName)
	dp := draining.peer()
	dp.Draining = true
	for _, n := range []*node{draining, live} {
		n.src.setPeers(f.peer())
	}
	f.src.setPeers(dp, live.peer())
	draining.start()
	live.start()
	f.start()
	// The draining holder sorts after the live one although its id is lower.
	eventually(t, "f's map", func() bool { return holdersOf(f, appName) == "edge-b,edge-a" })

	// A stream already open on the draining holder survives its listener closing...
	f.src.setPeers(dp)
	eventually(t, "f to forget the live holder", func() bool { return holdersOf(f, appName) == "edge-a" })
	conn, err := upgrade(t, f, appName, c.holder.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := echoOnce(conn, "before"); err != nil {
		t.Fatal(err)
	}
	_ = draining.ln.Close()
	if err := echoOnce(conn, "after the drain began"); err != nil {
		t.Fatalf("existing stream cut by the drain: %v", err)
	}
	// ...and once its HTTP server can no longer accept it refuses new forwards with draining.
	eventually(t, "draining refusal", func() bool {
		status, _, _, err := visit(t, f, appName, c.page.pool)
		return err == nil && status == 503
	})
	if status, _, _, err := visit(t, f, appName, c.page.pool); err != nil || status != 503 {
		t.Fatalf("visit to a draining holder: %d %v", status, err)
	}
}

func TestForwardPerPeerStreamCap(t *testing.T) {
	c := newTrio(t)
	holder, f := c.node(t, "edge-a"), c.node(t, "edge-b")
	holder.held.put(appName)
	holder.cfg = func(cfg *forward.Config) { cfg.MaxStreamsPerPeer = 2 }
	cluster(holder, f)
	eventually(t, "f's map", func() bool { return holdersOf(f, appName) == "edge-a" })
	var open []net.Conn
	for range 2 {
		conn, err := upgrade(t, f, appName, c.holder.pool)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		open = append(open, conn)
	}
	if status, _, _, err := visit(t, f, appName, c.page.pool); err != nil || status != 503 {
		t.Fatalf("third visitor over the cap: %d %v", status, err)
	}
	if err := echoOnce(open[0], "still open"); err != nil {
		t.Fatal(err)
	}
	// A slot frees when a forwarded visitor ends.
	_ = open[1].Close()
	eventually(t, "a slot to free", func() bool {
		conn, err := upgrade(t, f, appName, c.holder.pool)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

func TestForwardIdleLinkReconnects(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	a.held.put(appName)
	cluster(a, b)
	eventually(t, "b's map", func() bool { return holdersOf(b, appName) == "edge-a" })
	if status, _, _, err := visit(t, b, appName, c.holder.pool); err != nil || status != 200 {
		t.Fatalf("first visit: %d %v", status, err)
	}
	// The link a dials to b dies (a middlebox timed it out): both sides withdraw, then a redials.
	a.dropLinks()
	eventually(t, "the link to come back and b to relearn a's names", func() bool {
		return b.mesh.Stats().Links == 1 && holdersOf(b, appName) == "edge-a"
	})
	if status, _, _, err := visit(t, b, appName, c.holder.pool); err != nil || status != 200 {
		t.Fatalf("visit after the link came back: %d %v", status, err)
	}
	// A node that dials only on demand (b has the higher id) reconnects the same way.
	b.dropLinks()
	a.dropLinks()
	eventually(t, "forwarding to work again", func() bool {
		status, _, _, err := visit(t, b, appName, c.holder.pool)
		return err == nil && status == 200
	})
}

func TestMeshRestart(t *testing.T) {
	c := newTrio(t)
	f, h := c.node(t, "edge-a"), c.node(t, "edge-b")
	h.held.put(appName)
	f.src.setSeed(appName, "edge-b")
	cluster(f, h)
	eventually(t, "f's map", func() bool { return holdersOf(f, appName) == "edge-b" && f.mesh.Directory().Complete("edge-b") })

	// The holder's mesh stops: its watch entries are withdrawn, the server seed covers the gap.
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.mesh.Stop(stop); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the watch to be withdrawn", func() bool { return !f.mesh.Directory().Complete("edge-b") })
	if got := holdersOf(f, appName); got != "edge-b" {
		t.Fatalf("seed does not cover the gap: %q", got)
	}
	// The holder restarts (a new mesh on the same listener): f reconnects and the watch refills.
	h.start()
	eventually(t, "the watch to refill", func() bool { return f.mesh.Directory().Complete("edge-b") && f.mesh.Stats().Links == 1 })
	if status, _, _, err := visit(t, f, appName, c.holder.pool); err != nil || status != 200 {
		t.Fatalf("visit after the restart: %d %v", status, err)
	}
}

func TestHolderMapOver4096Names(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	names := make([]string, 20000)
	for i := range names {
		names[i] = fmt.Sprintf("n%05d.example.test", i)
	}
	a.held.put(names...)
	cluster(a, b)
	eventually(t, "20,000 names to arrive in chunks", func() bool { return b.mesh.Directory().Names() == 20000 && b.mesh.Directory().Complete("edge-a") })
	for _, i := range []int{0, 511, 512, 4095, 4096, 19999} {
		if got := holdersOf(b, names[i]); got != "edge-a" {
			t.Fatalf("holders of %s = %q", names[i], got)
		}
	}
	a.held.drop(names[:1000]...)
	eventually(t, "1,000 removals", func() bool { return b.mesh.Directory().Names() == 19000 })
	if got := holdersOf(b, names[0]); got != "" {
		t.Fatalf("removed name still held by %q", got)
	}
}

func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := ln.Accept(); accepted <- conn }()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return <-accepted, client
}

// echoTCP is a holder's raw TCP injector: it echoes the visitor and records the port it was asked for.
type echoTCP struct{ ports chan uint16 }

func (e *echoTCP) InjectTCP(conn net.Conn, port uint16) error {
	e.ports <- port
	go func() {
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	return nil
}

// A raw TCP key rides the same link: the visitor's bytes reach the holder's TCP injector for that port, and a holder
// without the injector refuses so the forwarder can move on.
func TestForwardTCPKeyReachesTCPInjector(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	key := nodemesh.TCPKey(24567)
	b.held.put(key)
	echo := &echoTCP{ports: make(chan uint16, 4)}
	b.cfg = func(cfg *forward.Config) { cfg.TCPInjector = echo }
	cluster(a, b)
	eventually(t, "a to learn the port", func() bool { return holdersOf(a, key) == "edge-b" && len(a.mesh.TCPPorts()) == 1 })
	if ports := a.mesh.TCPPorts(); ports[0] != 24567 {
		t.Fatalf("directory ports %v", ports)
	}
	server, client := tcpPair(t)
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- a.mesh.Forward(context.Background(), server, key); _ = server.Close() }()
	if err := echoOnce(client, "ping"); err != nil {
		t.Fatal(err)
	}
	if port := <-echo.ports; port != 24567 {
		t.Fatalf("injector asked for port %d", port)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A node that holds the key but has no TCP injector refuses; the forward then fails (no other holder).
	c2, d := c.node(t, "edge-c"), c.node(t, "edge-d")
	d.held.put(key)
	cluster(c2, d)
	eventually(t, "c to learn the port", func() bool { return holdersOf(c2, key) == "edge-d" })
	server2, client2 := tcpPair(t)
	defer client2.Close()
	err := c2.mesh.Forward(context.Background(), server2, key)
	var failure *forward.Failure
	if !errors.As(err, &failure) || !errors.Is(err, forward.ErrUnavailable) {
		t.Fatalf("forward to a holder without a TCP injector: %v", err)
	}
}
