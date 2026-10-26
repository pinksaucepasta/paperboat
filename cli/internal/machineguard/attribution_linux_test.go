//go:build linux

package machineguard

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func gatewayPair(t *testing.T, listener net.Listener, dialer *net.Dialer) (client, accepted net.Conn) {
	t.Helper()
	done := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- nil
			return
		}
		done <- conn
	}()
	client, err := dialer.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted = <-done
	if accepted == nil {
		t.Fatal("accept failed")
	}
	return client, accepted
}

func gatewayListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func ownDaemon(uid uint32) (string, bool) {
	if uid == uint32(os.Getuid()) {
		return "daemon", true
	}
	return "", false
}

func TestGatewayAttributesTheConnectingSocketsUID(t *testing.T) {
	client, accepted := gatewayPair(t, gatewayListener(t), &net.Dialer{})
	defer client.Close()
	defer accepted.Close()
	uid, daemon, err := attributeConnection(accepted, lookupOwnersINETDiag, ownDaemon)
	if err != nil || uid != uint32(os.Getuid()) || daemon != "daemon" {
		t.Fatalf("attribution = %d %q %v, want own uid", uid, daemon, err)
	}
}

func TestGatewayRefusesAUIDWithoutADaemon(t *testing.T) {
	client, accepted := gatewayPair(t, gatewayListener(t), &net.Dialer{})
	defer client.Close()
	defer accepted.Close()
	noDaemons := func(uint32) (string, bool) { return "", false }
	if _, _, err := attributeConnection(accepted, lookupOwnersINETDiag, noDaemons); !errors.Is(err, errAttributionNoDaemon) || !errors.Is(err, ErrAttribution) {
		t.Fatalf("attribution without a daemon = %v, want refused", err)
	}
}

func TestGatewayRefusesAPeerThatClosedBeforeLookup(t *testing.T) {
	for _, reset := range []bool{false, true} {
		client, accepted := gatewayPair(t, gatewayListener(t), &net.Dialer{})
		if reset {
			_ = client.(*net.TCPConn).SetLinger(0)
		}
		_ = client.Close()
		time.Sleep(10 * time.Millisecond)
		if uid, _, err := attributeConnection(accepted, lookupOwnersINETDiag, ownDaemon); !errors.Is(err, ErrAttribution) {
			t.Fatalf("closed peer (reset %v) attributed to %d", reset, uid)
		}
		_ = accepted.Close()
	}
}

// A client port reused after a reset reaches the gateway as the same tuple: each accepted connection is attributed
// only while its own connecting socket is live, and a stale accepted connection of a reset client never is.
func TestGatewayAttributionUnderTupleReuseChurn(t *testing.T) {
	listener := gatewayListener(t)
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}}
	var previous string
	for round := 0; round < 100; round++ {
		client, accepted := gatewayPair(t, listener, dialer)
		if uid, _, err := attributeConnection(accepted, lookupOwnersINETDiag, ownDaemon); err != nil || uid != uint32(os.Getuid()) {
			t.Fatalf("round %d: live connection = %d %v", round, uid, err)
		}
		if previous != "" && previous != accepted.RemoteAddr().String() {
			t.Fatalf("round %d: tuple was not reused", round)
		}
		previous = accepted.RemoteAddr().String()
		_ = client.(*net.TCPConn).SetLinger(0)
		_ = client.Close()
		time.Sleep(time.Millisecond)
		if _, _, err := attributeConnection(accepted, lookupOwnersINETDiag, ownDaemon); !errors.Is(err, ErrAttribution) {
			t.Fatalf("round %d: reset client still attributed: %v", round, err)
		}
		_ = accepted.Close()
	}
}

func TestGatewayAttributionRefusesAmbiguousAndInconsistentAnswers(t *testing.T) {
	tuple := socketTuple{Source: netip.MustParseAddrPort("127.0.0.1:40000"), Destination: netip.MustParseAddrPort("127.100.0.1:443")}
	other := socketTuple{Source: netip.MustParseAddrPort("127.0.0.1:40001"), Destination: tuple.Destination}
	cases := map[string]struct {
		answer []socketOwner
		want   error
	}{
		"ambiguous":    {[]socketOwner{{tuple, 1000, true}, {tuple, 1001, true}}, errAttributionAmbiguous},
		"inconsistent": {[]socketOwner{{other, 1000, true}}, errAttributionInconsistent},
		"time_wait":    {[]socketOwner{{tuple, 0, false}}, errAttributionNotFound},
		"missing":      {nil, errAttributionNotFound},
	}
	for name, tc := range cases {
		calls := 0
		lookup := func(socketTuple) ([]socketOwner, error) { calls++; return tc.answer, nil }
		start := time.Now()
		if _, err := attributeOwner(tuple, lookup); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
		if calls > attributionAttempts || time.Since(start) > 2*attributionBudget {
			t.Fatalf("%s: %d lookups in %s, want at most %d within %s", name, calls, time.Since(start), attributionAttempts, attributionBudget)
		}
		if name == "missing" && calls != attributionAttempts {
			t.Fatalf("a missing answer was looked up %d times, want %d", calls, attributionAttempts)
		}
	}
}

func TestGatewayAttributesOnlyIPv4Loopback(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	defer listener.Close()
	client, accepted := gatewayPair(t, listener, &net.Dialer{})
	defer client.Close()
	defer accepted.Close()
	if _, _, err := attributeConnection(accepted, lookupOwnersINETDiag, ownDaemon); !errors.Is(err, errAttributionUnsupported) {
		t.Fatalf("IPv6 connection = %v, want unsupported", err)
	}
}

// Device check (F23-02, pbvm2 two-user container): run the test binary as root with PB_TWO_USER_UIDS=<a>,<b>.
// Each client connects as its own OS user; every accepted connection is attributed to exactly that user.
func TestGatewayAttributesTwoOSUsers(t *testing.T) {
	uids := strings.Split(os.Getenv("PB_TWO_USER_UIDS"), ",")
	if os.Geteuid() != 0 || len(uids) != 2 {
		t.Skip("needs root and PB_TWO_USER_UIDS=<uid>,<uid>")
	}
	listener := gatewayListener(t)
	for round := 0; round < 50; round++ {
		for _, value := range uids {
			uid, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				t.Fatal(err)
			}
			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			client := exec.Command("bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/"+port+" && sleep 2")
			client.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)}}
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			accepted, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			got, err := attributeOwner(mustClientTuple(t, accepted), lookupOwnersINETDiag)
			if err != nil || got != uint32(uid) {
				t.Fatalf("round %d: connection of uid %d attributed to %d (%v)", round, uid, got, err)
			}
			_ = client.Process.Kill()
			_ = client.Wait()
			_ = accepted.Close()
		}
	}
}

func mustClientTuple(t *testing.T, accepted net.Conn) socketTuple {
	t.Helper()
	tuple, err := clientTuple(accepted)
	if err != nil {
		t.Fatal(err)
	}
	return tuple
}
