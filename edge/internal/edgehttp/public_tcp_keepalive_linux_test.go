package edgehttp

import (
	"syscall"
	"testing"
	"time"
)

func TestPublicTCPConnectionsGetKeepAlive(t *testing.T) {
	_, accepted := tcpPair(t)
	keepPublicTCPAlive(&replayTLSConn{Conn: accepted})
	raw, err := accepted.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var enabled, idle int
	if err := raw.Control(func(fd uintptr) {
		enabled, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		idle, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
	}); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || idle != int(publicTCPKeepAlive.Idle/time.Second) {
		t.Fatalf("keepalive enabled=%d idle=%ds", enabled, idle)
	}
}
