package selfhost

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func anyAddress(netip.Addr) bool { return true }

func TestProbeNodePortReportsWhatTheConnectionSaw(t *testing.T) {
	const nonce = "abcdefghijklmnop0123"
	echo := func(data string) func(context.Context, string, string) (net.Conn, error) {
		return func(context.Context, string, string) (net.Conn, error) {
			server, client := net.Pipe()
			go func() { _, _ = server.Write([]byte(data)); _ = server.Close() }()
			return client, nil
		}
	}
	fail := func(err error) func(context.Context, string, string) (net.Conn, error) {
		return func(context.Context, string, string) (net.Conn, error) { return nil, err }
	}
	for name, test := range map[string]struct {
		dial func(context.Context, string, string) (net.Conn, error)
		want PortProbe
	}{
		"echoed nonce":    {echo(nonce), PortProbe{Reachable: true, Detail: "connected"}},
		"another service": {echo("not the nonce at all"), PortProbe{Detail: "nonce_mismatch"}},
		"refused":         {fail(&net.OpError{Err: syscall.ECONNREFUSED}), PortProbe{Detail: "refused"}},
		"host firewall":   {fail(&net.OpError{Err: syscall.EHOSTUNREACH}), PortProbe{Detail: "unreachable"}},
		"cloud firewall":  {fail(context.DeadlineExceeded), PortProbe{Detail: "timeout"}},
		"other":           {fail(errors.New("boom")), PortProbe{Detail: "error"}},
	} {
		check := func(ctx context.Context, c net.Conn) PortProbe { return echoesNonce(ctx, c, nonce) }
		if got := probeNodePort(t.Context(), addrs("203.0.113.7"), "tcp", 25000, check, anyAddress, test.dial); got != test.want {
			t.Errorf("%s: got %+v, want %+v", name, got, test.want)
		}
	}
}

func TestProbeNodePortNeverDialsADisallowedAddress(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dialed a disallowed address")
		return nil, nil
	}
	for _, host := range []string{"127.0.0.1", "10.0.0.5", "169.254.169.254", "100.64.0.1", "::1", "fe80::1", "fd00::5"} {
		got := probeNodePort(t.Context(), addrs(host), "udp", 7274, answersSTUN, publicIngressIP, dial)
		if got != (PortProbe{Detail: "not_public"}) {
			t.Errorf("%s: got %+v", host, got)
		}
	}
}

func TestProbePortRefusesPortsAndNoncesOutsideWhatItMayDo(t *testing.T) {
	var s *Service
	const nonce = "abcdefghijklmnop0123"
	for _, test := range []struct {
		transport string
		port      int
		nonce     string
	}{
		{"tcp", 22, nonce}, {"tcp", 9999, nonce}, {"tcp", 60000, nonce}, {"tcp", 25000, "short"}, {"tcp", 25000, "abcdefghijklmnop 0123"},
		{"tcp", 22, ""}, {"tcp", 443, ""}, {"tcp", 7271, ""}, {"tcp", 7276, ""}, {"tcp", 7277, ""}, {"tcp", 9999, ""}, {"tcp", 25000, ""},
		{"tcp", 7272, nonce}, {"", 7272, ""}, {"quic", 7273, ""}, {"udp", 7273, ""}, {"udp", 7275, ""}, {"udp", 53, ""}, {"udp", 25000, nonce}, {"udp", 7274, nonce},
	} {
		if _, err := s.ProbePort(t.Context(), "credential", test.transport, test.port, test.nonce); !errors.Is(err, ErrPortProbeInvalid) {
			t.Errorf("%s %d nonce %q: err = %v, want ErrPortProbeInvalid", test.transport, test.port, test.nonce, err)
		}
	}
	// Allowed requests pass validation and stop at the nil service instead.
	for _, test := range []struct {
		transport string
		port      int
		nonce     string
	}{{"tcp", 25000, nonce}, {"tcp", 7272, ""}, {"tcp", 7275, ""}, {"udp", 7274, ""}} {
		if _, err := s.ProbePort(t.Context(), "credential", test.transport, test.port, test.nonce); errors.Is(err, ErrPortProbeInvalid) {
			t.Errorf("%s %d refused", test.transport, test.port)
		}
	}
}

func TestProbeNodePortConnectsToANodePortWithoutANonce(t *testing.T) {
	var network, address string
	dial := func(_ context.Context, n, a string) (net.Conn, error) {
		network, address = n, a
		_, client := net.Pipe()
		return client, nil
	}
	check := func(context.Context, net.Conn) PortProbe { return PortProbe{Reachable: true, Detail: "connected"} }
	got := probeNodePort(t.Context(), addrs("203.0.113.7"), "tcp", 7272, check, anyAddress, dial)
	if got != (PortProbe{Reachable: true, Detail: "connected"}) || network != "tcp" || address != "203.0.113.7:7272" {
		t.Fatalf("got %+v dialing %s %s", got, network, address)
	}
}

// stunServer answers each datagram with answer(request), or not at all when it returns nil, and counts requests.
func stunServer(t *testing.T, answer func([]byte) []byte) (net.Conn, *atomic.Int32) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	var requests atomic.Int32
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := server.ReadFrom(buf)
			if err != nil {
				return
			}
			requests.Add(1)
			if reply := answer(append([]byte(nil), buf[:n]...)); reply != nil {
				_, _ = server.WriteTo(reply, from)
			}
		}
	}()
	client, err := net.Dial("udp", server.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, &requests
}

// stunSuccess is a real binding success: header plus one XOR-MAPPED-ADDRESS attribute (IPv4 203.0.113.9:4242),
// as ~/ref/tailscale/net/stun/stun.go Response builds it.
func stunSuccess(request []byte) []byte {
	reply := append([]byte(nil), request[:20]...)
	binary.BigEndian.PutUint16(reply[0:], 0x0101)
	binary.BigEndian.PutUint16(reply[2:], 12)
	attribute := []byte{0x00, 0x20, 0x00, 0x08, 0x00, 0x01, 0, 0, 203, 0, 113, 9}
	binary.BigEndian.PutUint16(attribute[6:], 4242^0x2112)
	for i, b := range []byte{0x21, 0x12, 0xA4, 0x42} {
		attribute[8+i] ^= b
	}
	return append(reply, attribute...)
}

func TestAnswersSTUN(t *testing.T) {
	connected := PortProbe{Reachable: true, Detail: "connected"}
	mismatch := PortProbe{Detail: "nonce_mismatch"}
	for name, test := range map[string]struct {
		answer func([]byte) []byte
		want   PortProbe
	}{
		"binding success":      {stunSuccess, connected},
		"header only":          {func(r []byte) []byte { reply := stunSuccess(r)[:20]; reply[2], reply[3] = 0, 0; return reply }, connected},
		"wrong transaction id": {func(r []byte) []byte { reply := stunSuccess(r); reply[19] ^= 1; return reply }, mismatch},
		"wrong cookie":         {func(r []byte) []byte { reply := stunSuccess(r); reply[4] ^= 1; return reply }, mismatch},
		"binding error":        {func(r []byte) []byte { reply := stunSuccess(r); reply[1] = 0x11; return reply }, mismatch},
		"declared longer":      {func(r []byte) []byte { reply := stunSuccess(r); reply[3] = 16; return reply }, mismatch},
		"declared unaligned":   {func(r []byte) []byte { reply := stunSuccess(r); reply[3] = 11; return reply[:31] }, mismatch},
		"trailing bytes":       {func(r []byte) []byte { return append(stunSuccess(r), 0, 0, 0, 0) }, mismatch},
		"short":                {func(r []byte) []byte { return stunSuccess(r)[:19] }, mismatch},
		"oversized": {func(r []byte) []byte {
			reply := append(stunSuccess(r), make([]byte, 1100)...)
			binary.BigEndian.PutUint16(reply[2:], uint16(len(reply)-20))
			return reply
		}, mismatch},
		"attribute runs past the packet": {func(r []byte) []byte { reply := stunSuccess(r); reply[23] = 12; return reply }, mismatch},
		"valid first KiB, extra bytes after": {func(r []byte) []byte {
			reply := stunSuccess(r)
			software := stunResponseMax - len(reply) - 4 // a SOFTWARE attribute filling the KiB
			reply = append(reply, 0x80, 0x22, byte(software>>8), byte(software))
			reply = append(reply, make([]byte, software)...)
			binary.BigEndian.PutUint16(reply[2:], uint16(len(reply)-20))
			return append(reply, make([]byte, 200)...)
		}, mismatch},
		"fills the buffer exactly": {func(r []byte) []byte {
			reply := append(stunSuccess(r), make([]byte, stunResponseMax+1-32)...)
			binary.BigEndian.PutUint16(reply[2:], uint16(len(reply)-20))
			return reply
		}, mismatch},
	} {
		client, _ := stunServer(t, test.answer)
		if got := answersSTUN(t.Context(), client); got != test.want {
			t.Errorf("%s: got %+v, want %+v", name, got, test.want)
		}
	}
	dropFirst := 0
	client, requests := stunServer(t, func(r []byte) []byte {
		if dropFirst++; dropFirst == 1 {
			return nil
		}
		return stunSuccess(r)
	})
	if got := answersSTUN(t.Context(), client); got != connected || requests.Load() != 2 {
		t.Errorf("retry after one lost packet: got %+v after %d requests", got, requests.Load())
	}
	closed, _ := net.ListenPacket("udp", "127.0.0.1:0")
	_ = closed.Close()
	refused, err := net.Dial("udp", closed.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	if got := answersSTUN(t.Context(), refused); got != (PortProbe{Detail: "refused"}) {
		t.Errorf("closed port: got %+v", got)
	}
	silent, requests := stunServer(t, func([]byte) []byte { return nil })
	start := time.Now()
	if got := answersSTUN(t.Context(), silent); got != (PortProbe{Detail: "timeout"}) || time.Since(start) > 5*time.Second || requests.Load() != 2 {
		t.Errorf("silent port: got %+v after %v and %d requests", got, time.Since(start), requests.Load())
	}
}

func TestProbeNodePortReturnsPromptlyWhenCanceled(t *testing.T) {
	silent, requests := stunServer(t, func([]byte) []byte { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	got := probeNodePort(ctx, addrs("203.0.113.7"), "udp", 7274, answersSTUN, anyAddress, func(context.Context, string, string) (net.Conn, error) { return silent, nil })
	if got != (PortProbe{Detail: "timeout"}) || time.Since(start) > time.Second {
		t.Fatalf("got %+v after %v", got, time.Since(start))
	}
	time.Sleep(50 * time.Millisecond)
	if requests.Load() != 1 {
		t.Fatalf("sent %d requests; a canceled probe must not send another", requests.Load())
	}
	done, stop := context.WithCancel(t.Context())
	stop()
	dialed := false
	got = probeNodePort(done, addrs("203.0.113.7"), "tcp", 7272, func(context.Context, net.Conn) PortProbe { return PortProbe{Reachable: true} }, anyAddress, func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialed = true
		return nil, ctx.Err()
	})
	if got.Reachable || !dialed {
		t.Fatalf("canceled before dial: got %+v", got)
	}
}

// stalledConn counts writes and holds each read until release, then reports a read timeout, so a test
// controls exactly when the first STUN try ends.
type stalledConn struct {
	net.Conn
	writes  atomic.Int32
	wrote   chan struct{}
	release chan struct{}
}

func (c *stalledConn) Write(b []byte) (int, error) {
	if c.writes.Add(1) == 1 {
		close(c.wrote)
	}
	return len(b), nil
}
func (c *stalledConn) Read([]byte) (int, error)    { <-c.release; return 0, os.ErrDeadlineExceeded }
func (c *stalledConn) SetDeadline(time.Time) error { return nil }
func (c *stalledConn) Close() error                { return nil }

func TestAnswersSTUNSendsNoRetryAfterCancel(t *testing.T) {
	conn := &stalledConn{wrote: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan PortProbe, 1)
	go func() { result <- answersSTUN(ctx, conn) }()
	<-conn.wrote
	cancel()
	close(conn.release)
	if got := <-result; got != (PortProbe{Detail: "timeout"}) || conn.writes.Load() != 1 {
		t.Fatalf("got %+v after %d writes; a canceled probe must not retry", got, conn.writes.Load())
	}
}

func addrs(ips ...string) []netip.Addr {
	var out []netip.Addr
	for _, ip := range ips {
		out = append(out, netip.MustParseAddr(ip))
	}
	return out
}
