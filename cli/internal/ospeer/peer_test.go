//go:build linux || darwin

package ospeer

import (
	"errors"
	"net"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// A client that connects and hangs up before the server reads its credentials is an ordinary event.
// Linux still answers from SO_PEERCRED; macOS refuses LOCAL_PEERPID with ENOTCONN. That refusal must
// stay a plain disconnect, not an "unverifiable peer" fault.
func TestPeerThatHungUpIsADisconnectNotAnUnverifiablePeer(t *testing.T) {
	path := filepath.Join(shorttemp.Dir(t), "peer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	identity, err := Get(server)
	if err == nil {
		if identity.PID <= 0 {
			t.Fatalf("identity without a process: %+v", identity)
		}
		return
	}
	if !errors.Is(err, syscall.ENOTCONN) || errors.Is(err, ErrUnverified) {
		t.Fatalf("hung-up peer error = %v, want a plain ENOTCONN disconnect", err)
	}
}
