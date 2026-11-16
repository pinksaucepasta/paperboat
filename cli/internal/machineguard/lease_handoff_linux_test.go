//go:build linux

package machineguard

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLeaseFDNameRoundTripAndRefusals(t *testing.T) {
	l := &guardedLease{uid: "1001", ip: "127.100.0.9", port: 2222, hostname: "hp.local.example.test"}
	uid, ip, port, host, err := parseLeaseFDName(leaseFDName(l))
	if err != nil || uid != "1001" || ip != "127.100.0.9" || port != 2222 || host != "hp.local.example.test" {
		t.Fatalf("round trip: %v %v %v %v %v", uid, ip, port, host, err)
	}
	for _, bad := range []string{"", "pbl|1|127.100.0.9|2222", "x|1|127.100.0.9|22|h", "pbl|u|127.100.0.9|22|h", "pbl|1|nope|22|h", "pbl|1|127.100.0.9|0|h", "pbl|1|127.100.0.9|22|"} {
		if _, _, _, _, err := parseLeaseFDName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// systemd's descriptor store: the lease's socket and its name reach NOTIFY_SOCKET in one datagram.
func TestStoreLeaseSendsTheListeningSocketToSystemd(t *testing.T) {
	notify := filepath.Join(t.TempDir(), "notify")
	server, err := net.ListenPacket("unixgram", notify)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	t.Setenv("NOTIFY_SOCKET", notify)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	lease := &guardedLease{uid: "1000", ip: "127.0.0.1", port: port, hostname: "hp.local.example.test", listener: listener}
	if err := storeLease(lease); err != nil {
		t.Fatal(err)
	}
	buf, oob := make([]byte, 512), make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := server.(*net.UnixConn).ReadMsgUnix(buf, oob)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(buf[:n]), "FDSTORE=1\nFDNAME="+leaseFDName(lease); got != want {
		t.Fatalf("datagram %q, want %q", got, want)
	}
	messages, _ := unix.ParseSocketControlMessage(oob[:oobn])
	fds, err := unix.ParseUnixRights(&messages[0])
	if err != nil || len(fds) != 1 {
		t.Fatalf("rights: %v %v", fds, err)
	}
	stored, err := net.FileListener(os.NewFile(uintptr(fds[0]), "stored"))
	if err != nil || stored.Addr().String() != listener.Addr().String() {
		t.Fatalf("stored socket %v, %v", stored, err)
	}
	stored.Close()
	forgetLease(lease)
	n, _, _, _, err = server.(*net.UnixConn).ReadMsgUnix(buf, oob)
	if err != nil || string(buf[:n]) != "FDSTOREREMOVE=1\nFDNAME="+leaseFDName(lease) {
		t.Fatalf("remove datagram %q %v", buf[:n], err)
	}
}

// A guard replacement: the socket the old guard stored is inherited by the new one, keeps accepting connects while
// no guard owns it, and is handed to the daemon that re-acquires the same listener; another user is refused it.
func TestInheritedLeaseKeepsAcceptingAndIsHandedToItsDaemon(t *testing.T) {
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	host := "hp." + testBrowserDomain
	listener, err := net.Listen("tcp4", "127.100.0.77:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	old := &guardedLease{uid: "1000", ip: "127.100.0.77", port: port, hostname: host, listener: listener}
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	// The old guard exits without shutting the socket down; only the descriptor store keeps it.
	listener.Close()
	inherited := leasesFromFiles([]*os.File{file}, []string{leaseFDName(old)})
	if len(inherited) != 1 || inherited[0].owner != nil {
		t.Fatalf("inherited %v", inherited)
	}
	g := &guardServer{
		cfg:         Config{StateDir: state, LoopbackCIDR: "127.100.0.0/16"},
		names:       map[controlConn]map[string]string{},
		connections: map[controlConn]string{},
		leases:      map[string]*guardedLease{net.JoinHostPort("127.100.0.77", strconv.Itoa(port)): inherited[0]},
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.100.0.77", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatalf("connect while no guard owns the socket: %v", err)
	}
	conn.Close()
	other, daemon := &namesTestConn{id: 2}, &namesTestConn{id: 1}
	request := request{Operation: "acquire", Hostname: host, IP: "127.100.0.77", Port: port}
	if _, err := g.handle(t.Context(), other, "1001", request); err == nil {
		t.Fatal("another OS user was handed the inherited listener")
	}
	got, err := g.handle(t.Context(), daemon, "1000", request)
	if err != nil || got != inherited[0].listener || inherited[0].owner != controlConn(daemon) {
		t.Fatalf("the daemon did not get its inherited listener: %v %v", got, err)
	}
	accepted := make(chan error, 1)
	go func() { _, err := got.Accept(); accepted <- err }()
	conn, err = net.DialTimeout("tcp4", net.JoinHostPort("127.100.0.77", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("daemon accept: %v", err)
	}
}

func TestUnclaimedInheritedLeasesAreReleased(t *testing.T) {
	previous := applyPolicy
	applyPolicy = func(context.Context, Config, []*guardedLease) error { return nil }
	t.Cleanup(func() { applyPolicy = previous })
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	claimed := &namesTestConn{id: 1}
	g := &guardServer{cfg: Config{LoopbackCIDR: "127.0.0.0/8"}, failures: make(chan error, 1), leases: map[string]*guardedLease{
		"orphan":  {uid: "1000", listener: listener},
		"claimed": {uid: "1000", owner: claimed, listener: listener},
	}}
	g.releaseUnclaimed()
	if _, ok := g.leases["orphan"]; ok || g.leases["claimed"] == nil {
		t.Fatalf("leases after release: %v", g.leases)
	}
}

// systemctl disable on the guard unit flushed the descriptor store (measured on hp: store 35 to 0, then the new
// guard started with no rules for 3 s): the install sequence restarts the unit and never disables it.
func TestGuardInstallKeepsTheDescriptorStore(t *testing.T) {
	for _, args := range guardInstallCommands {
		if args[0] == "disable" || args[0] == "stop" {
			t.Fatalf("install runs systemctl %v, which flushes the guard's descriptor store", args)
		}
	}
	if last := guardInstallCommands[len(guardInstallCommands)-1]; last[0] != "restart" {
		t.Fatalf("install ends with %v, want a restart that passes the stored descriptors on", last)
	}
}
