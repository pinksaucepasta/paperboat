package machineguard

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func tcpTable(count uint32, rows ...[]byte) []byte {
	table := binary.LittleEndian.AppendUint32(nil, count)
	table = append(table, 0, 0, 0, 0)
	for _, row := range rows {
		table = append(table, row...)
	}
	return table
}

func tcpRow(local, remote netip.AddrPort, state, pid uint32, bound int64) []byte {
	row := make([]byte, tcpRowOwnerModuleLen)
	binary.LittleEndian.PutUint32(row, state)
	copy(row[4:], local.Addr().AsSlice())
	binary.BigEndian.PutUint16(row[8:], local.Port())
	copy(row[12:], remote.Addr().AsSlice())
	binary.BigEndian.PutUint16(row[16:], remote.Port())
	binary.LittleEndian.PutUint32(row[20:], pid)
	binary.LittleEndian.PutUint64(row[24:], uint64(bound))
	return row
}

// The owner of a gateway connection is the connecting socket's process, never the guard's accepted socket (the
// same connection seen from the other end) or another connection to the same port.
func TestTCPTableAnswersForTheConnectingSocketOnly(t *testing.T) {
	client, gateway := netip.MustParseAddrPort("127.0.0.1:50123"), netip.MustParseAddrPort("127.100.0.1:443")
	other := netip.MustParseAddrPort("127.0.0.1:50124")
	tuple := socketTuple{Source: client, Destination: gateway}
	table := tcpTable(3, tcpRow(gateway, client, tcpStateEstablished, 4, 90), tcpRow(other, gateway, tcpStateEstablished, 700, 95), tcpRow(client, gateway, tcpStateEstablished, 4321, 100))
	owners, err := matchTCPOwners(table, tuple)
	if err != nil || len(owners) != 1 || owners[0].UID != 4321 || owners[0].bound != 100 || !owners[0].Established || owners[0].Tuple != tuple {
		t.Fatalf("owners=%+v err=%v; want the connecting process 4321 only", owners, err)
	}
	closing, err := matchTCPOwners(tcpTable(1, tcpRow(client, gateway, 11, 4321, 100)), tuple) // TIME_WAIT
	if err != nil || len(closing) != 1 || closing[0].Established || closing[0].Live {
		t.Fatalf("time-wait row=%+v err=%v; want a row that is neither established nor live", closing, err)
	}
	for name, broken := range map[string][]byte{"short": {1, 0, 0}, "truncated": table[:len(table)-1], "count beyond rows": tcpTable(4, table[tcpTableHeaderLen:])} {
		if owners, err := matchTCPOwners(broken, tuple); err == nil {
			t.Fatalf("%s table answered %+v; want an error", name, owners)
		}
	}
}

func tcp6Row(local, remote netip.AddrPort, state, pid uint32, bound int64) []byte {
	row := make([]byte, tcp6RowOwnerModuleLen)
	copy(row, local.Addr().AsSlice())
	binary.BigEndian.PutUint16(row[20:], local.Port())
	copy(row[24:], remote.Addr().AsSlice())
	binary.BigEndian.PutUint16(row[44:], remote.Port())
	binary.LittleEndian.PutUint32(row[48:], state)
	binary.LittleEndian.PutUint32(row[52:], pid)
	binary.LittleEndian.PutUint64(row[56:], uint64(bound))
	return row
}

// An IPv6 loopback client (the preview proxy listens on ::1) is attributed from the IPv6 table.
func TestTCP6TableAnswersForTheConnectingSocketOnly(t *testing.T) {
	client, gateway := netip.MustParseAddrPort("[::1]:50123"), netip.MustParseAddrPort("[::1]:8443")
	tuple := socketTuple{Source: client, Destination: gateway}
	table := tcpTable(2, tcp6Row(gateway, client, tcpStateEstablished, 4, 90), tcp6Row(client, gateway, tcpStateEstablished, 4321, 100))
	owners, err := matchTCPOwners(table, tuple)
	if err != nil || len(owners) != 1 || owners[0].UID != 4321 || owners[0].bound != 100 || !owners[0].Established {
		t.Fatalf("owners=%+v err=%v; want the connecting process 4321 only", owners, err)
	}
}

// F23-02: no row, two rows, a closing socket, a failed read and a process that cannot be the one that bound the
// connection are all refused; the user is looked up only for a single established owner.
func TestWindowsAttributionFailsClosed(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	tuple, _ := clientTuple(accepted)
	owner := func(established bool) tcpOwner {
		return tcpOwner{bound: 100, socketOwner: socketOwner{Tuple: tuple, UID: 4321, Established: established}}
	}
	halfClosed := tcpOwner{bound: 100, socketOwner: socketOwner{Tuple: tuple, UID: 4321, Live: true}}
	user := func(pid uint32, bound int64) (string, error) {
		if pid != 4321 || bound != 100 {
			t.Fatalf("user looked up for pid %d bound %d", pid, bound)
		}
		return "S-1-5-21-1-2-3-1001", nil
	}
	if sid, err := attributeSID(accepted, func(socketTuple) ([]tcpOwner, error) { return []tcpOwner{owner(true)}, nil }, user, false); err != nil || sid != "S-1-5-21-1-2-3-1001" {
		t.Fatalf("single established owner: %q, %v", sid, err)
	}
	refused := map[string]func(socketTuple) ([]tcpOwner, error){
		"no row":      func(socketTuple) ([]tcpOwner, error) { return nil, nil },
		"two rows":    func(socketTuple) ([]tcpOwner, error) { return []tcpOwner{owner(true), owner(true)}, nil },
		"closing":     func(socketTuple) ([]tcpOwner, error) { return []tcpOwner{owner(false)}, nil },
		"failed read": func(socketTuple) ([]tcpOwner, error) { return nil, errors.New("table unavailable") },
	}
	for name, lookup := range refused {
		sid, err := attributeSID(accepted, lookup, func(uint32, int64) (string, error) {
			t.Fatalf("%s: the user was looked up", name)
			return "", nil
		}, false)
		if sid != "" || !errors.Is(err, ErrAttribution) {
			t.Fatalf("%s: %q, %v; want a refusal", name, sid, err)
		}
	}
	halfClosedLookup := func(socketTuple) ([]tcpOwner, error) { return []tcpOwner{halfClosed}, nil }
	if sid, err := attributeSID(accepted, halfClosedLookup, user, true); err != nil || sid != "S-1-5-21-1-2-3-1001" {
		t.Fatalf("a half-closed client for the proxy: %q, %v", sid, err)
	}
	if sid, err := attributeSID(accepted, halfClosedLookup, user, false); sid != "" || !errors.Is(err, ErrAttribution) {
		t.Fatalf("a half-closed client for the gateway: %q, %v; want a refusal", sid, err)
	}
	if processBoundConnection(101, 100) || processBoundConnection(100, 0) || !processBoundConnection(100, 100) {
		t.Fatal("a process created after the bind, or a connection without a bind time, must be refused")
	}
}

func currentUserSID(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid.String()
}

// The real kernel table: a connection from this process and one from a process started a moment before it
// connected are attributed to this user; a pid whose process started after the bind is refused (pid reuse), and
// so is a connection whose client is gone.
func TestConnectionOwnerSIDOnTheLiveKernel(t *testing.T) {
	if address := os.Getenv("PB_ATTRIBUTION_DIAL"); address != "" {
		conn, err := net.Dial("tcp4", address)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Read(make([]byte, 1)) // until the parent closes it
		return
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	self := currentUserSID(t)

	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if sid, err := connectionOwnerSID(accepted); err != nil || sid != self {
		t.Fatalf("own connection attributed to %q, %v; want %s", sid, err, self)
	}
	tuple, _ := clientTuple(accepted)
	owners, err := lookupTCPOwners(tuple)
	if err != nil || len(owners) != 1 || owners[0].UID != uint32(os.Getpid()) {
		t.Fatalf("owners=%+v err=%v; want this process", owners, err)
	}
	v6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer v6.Close()
	v6client, err := net.Dial("tcp6", v6.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer v6client.Close()
	v6accepted, err := v6.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer v6accepted.Close()
	if sid, err := connectionOwnerSID(v6accepted); err != nil || sid != self {
		t.Fatalf("IPv6 loopback connection attributed to %q, %v; want %s", sid, err, self)
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user); err != nil {
		t.Fatal(err)
	}
	if sid, err := processUserSID(uint32(os.Getpid()), created.Nanoseconds()/100+116444736000000000-1); !errors.Is(err, ErrAttribution) {
		t.Fatalf("a bind one tick before this process started was attributed to %q, %v", sid, err)
	}
	started := time.Now()
	for range 100 {
		if _, err := lookupTCPOwners(tuple); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("one lookup of the kernel TCP table: %s", time.Since(started)/100)

	// A dual-stack socket (.NET's default) reaches an IPv4 address as ::ffff:a.b.c.d; it is the same user's.
	dual, err := windows.Socket(windows.AF_INET6, windows.SOCK_STREAM, windows.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.Closesocket(dual)
	if err := windows.SetsockoptInt(dual, windows.IPPROTO_IPV6, windows.IPV6_V6ONLY, 0); err != nil {
		t.Fatal(err)
	}
	gateway := listener.Addr().(*net.TCPAddr).AddrPort()
	if err := windows.Connect(dual, &windows.SockaddrInet6{Port: int(gateway.Port()), Addr: gateway.Addr().As16()}); err != nil {
		t.Fatal(err)
	}
	mapped, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer mapped.Close()
	if sid, err := connectionOwnerSID(mapped); err != nil || sid != self {
		t.Fatalf("dual-stack connection attributed to %q, %v; want %s", sid, err, self)
	}

	client.Close()
	time.Sleep(100 * time.Millisecond)
	if sid, err := connectionOwnerSID(accepted); !errors.Is(err, ErrAttribution) {
		t.Fatalf("closed client attributed to %q, %v; want a refusal", sid, err)
	}

	for round := 0; round < 5; round++ {
		child := exec.Command(os.Args[0], "-test.run=^TestConnectionOwnerSIDOnTheLiveKernel$")
		child.Env = append(os.Environ(), "PB_ATTRIBUTION_DIAL="+listener.Addr().String())
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		fresh, err := acceptWithin(listener, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		sid, err := connectionOwnerSID(fresh)
		fresh.Close()
		_ = child.Wait()
		if err != nil || sid != self {
			t.Fatalf("round %d: a just-started process's connection attributed to %q, %v; want %s", round, sid, err, self)
		}
	}
}

// F23-02: the Windows twin of the Linux/darwin churn tests. A client port reused after a reset reaches the
// gateway as the same tuple: the live connection is attributed to this user's SID and the reset one is
// refused, never another SID. 100 rounds on tcp4 and tcp6 loopback.
func TestConnectionOwnerSIDUnderTupleReuseChurn(t *testing.T) {
	self := currentUserSID(t)
	for _, network := range []string{"tcp4", "tcp6"} {
		host := map[string]string{"tcp4": "127.0.0.1", "tcp6": "::1"}[network]
		listen := host
		if network == "tcp6" {
			listen = "[::1]"
		}
		listener, err := net.Listen(network, listen+":0")
		if err != nil {
			t.Skipf("no %s loopback", network)
		}
		probe, err := net.Listen(network, listen+":0")
		if err != nil {
			t.Fatal(err)
		}
		port := probe.Addr().(*net.TCPAddr).Port
		_ = probe.Close()
		dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(host), Port: port}}
		var previous string
		for round := 0; round < 100; round++ {
			accepted := make(chan net.Conn, 1)
			go func() {
				c, _ := listener.Accept()
				accepted <- c
			}()
			client, err := dialer.Dial(network, listener.Addr().String())
			if err != nil {
				t.Fatalf("%s round %d: %v", network, round, err)
			}
			server := <-accepted
			if sid, err := connectionOwnerSID(server); err != nil || sid != self {
				t.Fatalf("%s round %d: live connection attributed to %q, %v; want %s", network, round, sid, err, self)
			}
			if previous != "" && previous != server.RemoteAddr().String() {
				t.Fatalf("%s round %d: tuple was not reused", network, round)
			}
			previous = server.RemoteAddr().String()
			_ = client.(*net.TCPConn).SetLinger(0)
			_ = client.Close()
			time.Sleep(10 * time.Millisecond) // let the reset land; a missing row is retried within the lookup budget
			if sid, err := connectionOwnerSID(server); !errors.Is(err, ErrAttribution) {
				t.Fatalf("%s round %d: reset client attributed to %q, %v; want a refusal", network, round, sid, err)
			}
			_ = server.Close()
		}
		_ = listener.Close()
	}
}
