package derpquic

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/quic-go/quic-go"
	"tailscale.com/types/key"
)

// A second loopback address stands in for a second visitor; Linux answers on all of 127.0.0.0/8.
const otherSource = "127.0.0.2"

// A source that opens connections and never authorizes holds at most preGrantPerAddress of them; the
// connection past the bound is refused, and another source still connects and authorizes.
func TestQUICPreGrantBoundsUnauthenticatedConnectionsPerSource(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	cert := certificate(t)
	socket, err := net.ListenPacket("udp4", otherSource+":0")
	if err != nil {
		t.Skip("second loopback address unavailable:", err)
	}
	transport := &quic.Transport{Conn: socket}
	defer transport.Close()
	remote, _ := net.ResolveUDPAddr("udp4", address)
	var held []*quic.Conn
	for i := 0; i <= preGrantPerAddress; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), waitBound)
		c, err := transport.Dial(ctx, remote, &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}, NextProtos: []string{ALPN}}, quicConfig())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	eventually(t, func() bool { return s.Snapshot().Denied == 1 })
	select {
	case <-held[preGrantPerAddress].Context().Done():
	case <-time.After(waitBound):
		t.Fatal("the connection past one source's share stayed open")
	}
	for _, c := range held[:preGrantPerAddress] {
		if c.Context().Err() != nil {
			t.Fatal("a connection within the share was closed")
		}
	}
	k := key.NewNode().Public()
	connect(t, f.client(address, cert, f.grant(k, cert, "second-source")))
}

func TestWSSPreGrantBoundsUnauthenticatedConnectionsPerSource(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	cert := certificate(t)
	k := key.NewNode().Public()
	wss, address := f.wss(s, cert, f.grant(k, cert, "second-source"), nil)
	client := &http.Client{Transport: &http.Transport{
		DialContext:     (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(otherSource)}}).DialContext,
		TLSClientConfig: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}},
	}}
	dial := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), waitBound)
		defer cancel()
		c, resp, err := websocket.Dial(ctx, "wss://"+address, &websocket.DialOptions{HTTPClient: client, Subprotocols: []string{WSSSubprotocol}})
		if err == nil {
			t.Cleanup(func() { _ = c.CloseNow() })
		} else if resp != nil && resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("refused with %d, want 503", resp.StatusCode)
		}
		return err
	}
	for i := 0; i < preGrantPerAddress; i++ {
		if err = dial(); err != nil {
			t.Fatalf("connection %d within the share refused: %v", i, err)
		}
	}
	if err = dial(); err == nil {
		t.Fatal("the connection past one source's share was accepted")
	}
	if s.Snapshot().Denied != 1 {
		t.Fatalf("denied = %d, want 1", s.Snapshot().Denied)
	}
	connectWSS(t, wss)
}
