package selfhost

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
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
		if got := probeNodePort(t.Context(), "203.0.113.7", 25000, nonce, anyAddress, test.dial); got != test.want {
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
		got := probeNodePort(t.Context(), host, 25000, "abcdefghijklmnop0123", publicIngressIP, dial)
		if got != (PortProbe{Detail: "not_public"}) {
			t.Errorf("%s: got %+v", host, got)
		}
	}
}

func TestProbePortRefusesPortsAndNoncesOutsideWhatItMayDo(t *testing.T) {
	var s *Service
	for _, test := range []struct {
		port  int
		nonce string
	}{{22, "abcdefghijklmnop0123"}, {9999, "abcdefghijklmnop0123"}, {60000, "abcdefghijklmnop0123"}, {25000, "short"}, {25000, "abcdefghijklmnop 0123"}} {
		if _, err := s.ProbePort(t.Context(), "credential", test.port, test.nonce); !errors.Is(err, ErrPortProbeInvalid) {
			t.Errorf("port %d nonce %q: err = %v, want ErrPortProbeInvalid", test.port, test.nonce, err)
		}
	}
}
