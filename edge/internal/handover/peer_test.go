//go:build linux

package handover

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func connectedPair(t *testing.T) (client, server *net.UnixConn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	accepted := make(chan *net.UnixConn, 1)
	go func() { c, _ := l.AcceptUnix(); accepted <- c }()
	client, err = net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

// Certificate private keys cross this socket: a peer that is another user is refused before
// anything is read or sent.
func TestPeerOfAnotherUserIsRefused(t *testing.T) {
	client, server := connectedPair(t)
	if err := requirePeerUID(server, os.Geteuid()); err != nil {
		t.Fatalf("same user refused: %v", err)
	}
	if err := requirePeerUID(server, os.Geteuid()+1); err == nil {
		t.Fatal("a peer of another user was accepted")
	}
	// The serving side refuses the exchange and sends nothing: the peer reads EOF, not sockets.
	old := peerUID
	peerUID = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { peerUID = old })
	go func() { _, _ = client.Write([]byte(`{"op":"hello"}`)) }()
	done := make(chan bool, 1)
	go func() {
		done <- offer(server, "epoch_0001", func() ([][]byte, error) { return [][]byte{[]byte("key")}, nil })
	}()
	select {
	case released := <-done:
		if released {
			t.Fatal("offer released to a refused peer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("offer did not refuse the peer")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := client.Read(make([]byte, 64)); n != 0 || err == nil {
		t.Fatalf("a refused peer received %d bytes", n)
	}
}
