package selfhost

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
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
		check := func(c net.Conn) PortProbe { return echoesNonce(c, nonce) }
		if got := probeNodePort(t.Context(), "203.0.113.7", "tcp", 25000, check, anyAddress, test.dial); got != test.want {
			t.Errorf("%s: got %+v, want %+v", name, got, test.want)
		}
	}
}

func TestProbeNodePortNeverDialsADisallowedAddress(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dialed a disallowed address")
		return nil, nil
	}
	for _, host := range []string{"127.0.0.1", "10.0.0.5", "169.254.169.254", "100.64.0.1"} {
		got := probeNodePort(t.Context(), host, "udp", 7274, answersSTUN, publicIngressIP, dial)
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
	check := func(net.Conn) PortProbe { return PortProbe{Reachable: true, Detail: "connected"} }
	got := probeNodePort(t.Context(), "203.0.113.7", "tcp", 7272, check, anyAddress, dial)
	if got != (PortProbe{Reachable: true, Detail: "connected"}) || network != "tcp" || address != "203.0.113.7:7272" {
		t.Fatalf("got %+v dialing %s %s", got, network, address)
	}
}

func TestAnswersSTUN(t *testing.T) {
	listen := func(t *testing.T, answer func([]byte) []byte) net.Conn {
		server, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		go func() {
			buf := make([]byte, 1500)
			for {
				n, from, err := server.ReadFrom(buf)
				if err != nil {
					return
				}
				if reply := answer(buf[:n]); reply != nil {
					_, _ = server.WriteTo(reply, from)
				}
			}
		}()
		client, err := net.Dial("udp", server.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	success := func(request []byte) []byte {
		reply := append([]byte(nil), request[:20]...)
		binary.BigEndian.PutUint16(reply, 0x0101)
		return reply
	}
	if got := answersSTUN(listen(t, success)); got != (PortProbe{Reachable: true, Detail: "connected"}) {
		t.Errorf("STUN server: got %+v", got)
	}
	dropFirst := 0
	if got := answersSTUN(listen(t, func(r []byte) []byte {
		if dropFirst++; dropFirst == 1 {
			return nil
		}
		return success(r)
	})); !got.Reachable {
		t.Errorf("retry after one lost packet: got %+v", got)
	}
	wrongID := func(r []byte) []byte { reply := success(r); reply[19] ^= 1; return reply }
	if got := answersSTUN(listen(t, wrongID)); got != (PortProbe{Detail: "nonce_mismatch"}) {
		t.Errorf("wrong transaction id: got %+v", got)
	}
	closed, _ := net.ListenPacket("udp", "127.0.0.1:0")
	_ = closed.Close()
	client, err := net.Dial("udp", closed.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := answersSTUN(client); got != (PortProbe{Detail: "refused"}) {
		t.Errorf("closed port: got %+v", got)
	}
	start := time.Now()
	if got := answersSTUN(listen(t, func([]byte) []byte { return nil })); got != (PortProbe{Detail: "timeout"}) || time.Since(start) > 5*time.Second {
		t.Errorf("silent port: got %+v after %v", got, time.Since(start))
	}
}
