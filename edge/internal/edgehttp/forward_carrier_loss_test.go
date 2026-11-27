package edgehttp

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// httpDecisionNode is an edge whose authority still holds a valid http decision for host but no carrier serves it here:
// the state an edge is in after the machine's carrier left it and before the server's next snapshot drops the decision.
func httpDecisionNode(t *testing.T, host string, carrier bool) (*tcpNode, net.Listener, net.Listener) {
	t.Helper()
	n := newTCPNode(t, false, nil)
	d := publicTCPDecision(time.Now().UTC(), 443)
	d.Binding.Protocol, d.Binding.ListenerID, d.Binding.Hostname, d.Binding.PublicPort = "http", "", host, 0
	d.Binding.PathPrefix, d.Binding.OriginScheme = "/", "http"
	if err := d.Validate(time.Now().UTC()); err != nil {
		t.Fatalf("fixture decision invalid: %v", err)
	}
	n.authority.set(d)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := n.p.WrapTLSListener(raw, func(name string) bool { return carrier && name == host }, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrapped.Close() })
	return n, raw, wrapped
}

func dialSNI(t *testing.T, address, host string) {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		_ = tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}).Handshake()
	}()
}

func TestHTTPSNameWithoutLocalCarrierIsForwardedToHolder(t *testing.T) {
	const host = "mild-icy-summit.preview.test"
	holder, _, holderTLS := httpDecisionNode(t, host, true)
	forwarder, raw, forwarderTLS := httpDecisionNode(t, host, false)
	dir := &directory{t: t, holders: map[string][]*tcpNode{host: {holder}}}
	forwarder.p.SetForwarding(ForwardHooks{Forwarder: dir})
	dialSNI(t, raw.Addr().String(), host)
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := holderTLS.Accept(); err == nil {
			accepted <- c
		}
	}()
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(5 * time.Second):
		t.Fatalf("the holder never served the visitor (forwarded %d)", dir.opened.Load())
	}
	_ = forwarderTLS
}

func TestHTTPSNameWithoutLocalCarrierOrHolderIsServedHere(t *testing.T) {
	const host = "mild-icy-summit.preview.test"
	forwarder, raw, forwarderTLS := httpDecisionNode(t, host, false)
	dir := &directory{t: t, holders: map[string][]*tcpNode{}}
	forwarder.p.SetForwarding(ForwardHooks{Forwarder: dir})
	dialSNI(t, raw.Addr().String(), host)
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := forwarderTLS.Accept(); err == nil {
			accepted <- c
		}
	}()
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("with no holder elsewhere the edge must answer the name itself, as before")
	}
	if dir.opened.Load() != 0 {
		t.Fatalf("forwarded %d visitors with no holder", dir.opened.Load())
	}
}

// A node whose carrier left refuses a forwarded visitor before the ack, so the forwarder tries the live holder.
func TestHTTPSNameWithoutLocalCarrierIsNotAdmittedForForwarding(t *testing.T) {
	const host = "mild-icy-summit.preview.test"
	stale, _, staleTLS := httpDecisionNode(t, host, false)
	live, _, liveTLS := httpDecisionNode(t, host, true)
	_, _ = stale, live
	if err := staleTLS.(*SharedTLSListener).Admit(host); err == nil {
		t.Fatal("a node without the carrier admitted a forwarded visitor")
	}
	if err := liveTLS.(*SharedTLSListener).Admit(host); err != nil {
		t.Fatalf("the holder refused: %v", err)
	}
}
