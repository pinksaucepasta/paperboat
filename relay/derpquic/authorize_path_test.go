package derpquic

import (
	"errors"
	"net"
	"strings"
	"testing"

	"tailscale.com/types/key"
)

// The production authorize methods (not a stub) decide what a failed re-authentication is: lost in the network, a
// grant that can never be framed, or fine. A network loss must stay retryable, because the QUIC revoke closes the
// connection with the admission code, which latches the carrier fatal for its lifetime.
func TestQUICAuthorizeSeparatesNetworkLossFromAnUnframeableGrant(t *testing.T) {
	f := newFixture(t)
	_, address, stop := f.start("127.0.0.1:0")
	defer stop()
	k, cert := key.NewNode().Public(), certificate(t)
	g := f.grant(k, cert, "a")
	c := f.client(address, cert, g)
	connect(t, c)
	c.mu.Lock()
	cc := c.current
	c.mu.Unlock()

	if err := cc.authorize(f.token(g)); err != nil {
		t.Fatalf("a re-authentication on a healthy connection failed: %v", err)
	}

	// A grant part that fits grantParts but not the frame once the namespace header is added: no network involved.
	cc.writeMu.Lock()
	cc.namespace = [32]byte{1}
	cc.writeMu.Unlock()
	err := cc.authorize(strings.Repeat("x", MaxControl))
	var fatal interface{ Fatal() bool }
	if errors.Is(err, errAuthorizeTransport) || !errors.As(err, &fatal) || !fatal.Fatal() {
		t.Fatalf("an unframeable grant gave %v: it must stay a fatal carrier error so it is revoked, never retried forever", err)
	}
	cc.writeMu.Lock()
	cc.namespace = [32]byte{}
	cc.writeMu.Unlock()

	// The connection is gone: the write fails in the network layer and must be reported as retryable.
	cc.conn.CloseWithError(0, "the network went away")
	err = cc.authorize(f.token(g))
	if !errors.Is(err, errAuthorizeTransport) {
		t.Fatalf("a re-authentication lost with the connection gave %v, want errAuthorizeTransport", err)
	}
	if errors.As(err, &fatal) && fatal.Fatal() {
		t.Fatalf("a lost re-authentication is fatal: %v", err)
	}
}

func TestWSSAuthorizeSeparatesNetworkLossFromAnUnframeableGrant(t *testing.T) {
	near, far := net.Pipe()
	cc := &wssClientConn{conn: near, namespace: [32]byte{1}}
	go func() { _, _ = far.Read(make([]byte, 1<<20)) }()
	err := cc.authorize(strings.Repeat("x", MaxControl))
	var fatal interface{ Fatal() bool }
	if errors.Is(err, errAuthorizeTransport) || !errors.As(err, &fatal) || !fatal.Fatal() {
		t.Fatalf("an unframeable grant gave %v on the WSS carrier, want a fatal carrier error", err)
	}
	far.Close()
	err = cc.authorize("a-grant")
	if !errors.Is(err, errAuthorizeTransport) {
		t.Fatalf("a re-authentication lost with the connection gave %v on the WSS carrier, want errAuthorizeTransport", err)
	}
}
