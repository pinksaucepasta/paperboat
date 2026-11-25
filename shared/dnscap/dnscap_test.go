package dnscap

import (
	"context"
	"net"
	"testing"
	"time"
)

// A nameserver that never answers is given up after dnsServerWait, not the resolver's 5 s, and the connection is
// still a packet connection (the resolver frames anything else as DNS over TCP).
func TestADeadNameserverIsGivenUpAfterTheCap(t *testing.T) {
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	conn, err := New().Dial(context.Background(), "udp", silent.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok := conn.(net.PacketConn); !ok {
		t.Fatalf("%T is not a packet connection", conn)
	}
	start := time.Now()
	_ = conn.SetDeadline(start.Add(5 * time.Second))
	if _, err := conn.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Read(make([]byte, 512))
	if waited := time.Since(start); err == nil || waited < dnsServerWait-200*time.Millisecond || waited > dnsServerWait+time.Second {
		t.Fatalf("read ended after %s with %v, want a timeout after %s", waited.Round(time.Millisecond), err, dnsServerWait)
	}
}
