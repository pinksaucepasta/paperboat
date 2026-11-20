package forward_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// helloVisitor is a visitor the forwarder was handed with its ClientHello and the socket address it was accepted from.
type helloVisitor struct {
	net.Conn
	hello  []byte
	remote net.Addr
}

func (v helloVisitor) ClientHello() []byte { return v.hello }
func (v helloVisitor) RemoteAddr() net.Addr {
	if v.remote != nil {
		return v.remote
	}
	return v.Conn.RemoteAddr()
}

// clientHello is the first flight a Go TLS client sends for name.
func clientHello(t *testing.T, name string) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _ = tls.Client(c1, &tls.Config{ServerName: name, InsecureSkipVerify: true}).Handshake() }()
	buf := make([]byte, 16<<10)
	n, err := c2.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

// The holder acks only when its own listener authority serves the name. b still lists the name as held (its watch
// and holdings say so) but its listener would not serve it: b refuses before the ack, f tries c, and the visitor
// never sees a reset or b's not-found page.
func TestForwardAckOnlyAfterAuthority(t *testing.T) {
	c := newTrio(t)
	b, d, f := c.node(t, "edge-b"), c.node(t, "edge-c"), c.node(t, "edge-f")
	d.held.put(appName)
	b.cfg = func(cfg *forward.Config) { cfg.Held = staticHeld{appName} } // announces the name its listener no longer serves
	cluster(b, d, f)
	eventually(t, "f to list both holders", func() bool { return holdersOf(f, appName) == "edge-b,edge-c" })
	status, body, _, err := visit(t, f, appName, c.holder.pool)
	if err != nil || status != 200 || !strings.Contains(body, "holder=edge-c") {
		t.Fatalf("visit with a first holder that cannot serve: %d %q %v", status, body, err)
	}
	if b.hits.Load() != 0 || b.mesh.Stats().Accepted != 0 || f.mesh.Stats().HolderMiss == 0 {
		t.Fatalf("b acked or served: hits=%d b=%+v f=%+v", b.hits.Load(), b.mesh.Stats(), f.mesh.Stats())
	}
}

type staticHeld []string

func (s staticHeld) Held() []string { return s }

// The hello names another host than the key the forwarder asked for: the holder refuses, nothing is served.
func TestForwardSNIKeyMismatchRefused(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	a.held.put(appName)
	b.held.put("app2.example.test")
	cluster(a, b)
	eventually(t, "b to list a", func() bool { return holdersOf(b, appName) == "edge-a" })
	server, client := tcpPair(t)
	defer client.Close()
	defer server.Close()
	err := b.mesh.Forward(context.Background(), helloVisitor{Conn: server, hello: clientHello(t, "app2.example.test")}, appName)
	var failure *forward.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("a hello for another name was forwarded: %v", err)
	}
	if a.hits.Load() != 0 || a.mesh.Stats().Accepted != 0 {
		t.Fatalf("holder acked a mismatched SNI: %+v", a.mesh.Stats())
	}
}

// A hello over nodemesh.MaxHello is not forwarded at all, and a hostname visitor without a hello neither.
func TestForwardHelloTooLargeRefused(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	a.held.put(appName)
	cluster(a, b)
	eventually(t, "b to list a", func() bool { return holdersOf(b, appName) == "edge-a" })
	for name, hello := range map[string][]byte{
		"too large": append(clientHello(t, appName), make([]byte, nodemesh.MaxHello)...),
		"none":      nil,
	} {
		server, client := tcpPair(t)
		err := b.mesh.Forward(context.Background(), helloVisitor{Conn: server, hello: hello}, appName)
		_, _ = client.Close(), server.Close()
		if !errors.Is(err, forward.ErrNoHolder) {
			t.Fatalf("%s hello: %v", name, err)
		}
	}
	if a.mesh.Stats().Accepted != 0 || b.mesh.Stats().Forwarded != 0 || b.mesh.Stats().Unreachable != 0 {
		t.Fatalf("a stream was opened: a=%+v b=%+v", a.mesh.Stats(), b.mesh.Stats())
	}
}

// The holder takes the visitor address only if it is a routable unicast one; the forwarder sends the accepted
// socket's address, so these are the addresses a compromised peer could claim.
func TestForwardVisitorAddressRejectsUnroutable(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	a.held.put(appName)
	cluster(a, b)
	eventually(t, "b to list a", func() bool { return holdersOf(b, appName) == "edge-a" })
	for _, claimed := range []string{"127.0.0.1:4000", "[::1]:4000", "0.0.0.0:4000", "[::]:4000", "224.0.0.1:4000", "169.254.1.1:4000", "[fe80::1]:4000", "[::ffff:127.0.0.1]:4000", "192.0.2.9:0"} {
		server, client := tcpPair(t)
		err := b.mesh.Forward(context.Background(), helloVisitor{Conn: server, hello: clientHello(t, appName), remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(claimed))}, appName)
		_, _ = client.Close(), server.Close()
		var failure *forward.Failure
		if err == nil || !errors.As(err, &failure) {
			t.Fatalf("visitor %s was forwarded: %v", claimed, err)
		}
	}
	if a.mesh.Stats().Accepted != 0 || a.hits.Load() != 0 {
		t.Fatalf("holder accepted an unroutable visitor: %+v", a.mesh.Stats())
	}
}

// The link outlives a directory change: once the forwarding peer is no longer in the holder's current peer set the
// holder refuses its Opens, even though the handshake that built the link was valid.
func TestForwardRefusesPeerNoLongerInDirectory(t *testing.T) {
	c := newTrio(t)
	a, b := c.node(t, "edge-a"), c.node(t, "edge-b")
	a.held.put(appName)
	a.cfg = func(cfg *forward.Config) { cfg.PeerInterval = time.Hour } // keeps the quiet change below from being refreshed away
	cluster(a, b)
	eventually(t, "b to list a", func() bool { return holdersOf(b, appName) == "edge-a" })
	if status, body, _, err := visit(t, b, appName, c.holder.pool); err != nil || status != 200 {
		t.Fatalf("visit while the peer is listed: %d %q %v", status, body, err)
	}
	a.mesh.SetPeersQuietly(nil)
	before := a.mesh.Stats().Accepted
	status, _, _, err := visit(t, b, appName, c.page.pool)
	if err != nil || status == 200 {
		t.Fatalf("holder served a peer that left the directory: %d %v", status, err)
	}
	if a.mesh.Stats().Accepted != before || a.hits.Load() != 1 {
		t.Fatalf("holder accepted after the peer left: %+v hits=%d", a.mesh.Stats(), a.hits.Load())
	}
}
