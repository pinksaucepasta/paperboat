package managedssh

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
)

// The bridge reaches sshd from the user's own loopback source (OpenSSH 9.8+ penalties are per source address), and
// where the system cannot bind it (macOS without the machine guard's aliases) from the shared 127.0.0.1 instead of
// failing the login.
func TestBridgeConnectsToSSHDFromTheUsersLoopbackSource(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	remote := make(chan netip.Addr, 1)
	go func() {
		connection, acceptErr := listener.AcceptTCP()
		if acceptErr != nil {
			remote <- netip.Addr{}
			return
		}
		remote <- connection.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
		_ = connection.Close()
	}()
	source := wire.LoopbackSource("user_a")
	want := source
	if probe, err := net.Listen("tcp4", net.JoinHostPort(source.String(), "0")); err != nil {
		want = netip.MustParseAddr("127.0.0.1")
	} else {
		_ = probe.Close()
	}
	bridgeSide, clientSide := net.Pipe()
	defer clientSide.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = BridgeSSH(context.Background(), bridgeSide, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port), Source: source}, time.Second)
	}()
	if got := <-remote; got != want {
		t.Fatalf("sshd saw the bridge from %s, want %s", got, want)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge did not end after both sides closed")
	}
}
