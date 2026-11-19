//go:build linux

package machineguard

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
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
// runInOwnLoopback reruns the calling test in a new user and network namespace with only loopback up, so the
// host's own machine guard (which rejects 127.100.0.0/16 except its leases) cannot refuse the test's dials. It
// (as root of that namespace: only it may bring loopback up) reports true when the rerun did the test; false means run it here (inside the namespace, or where the host
// allows no unprivileged namespace, such as GitHub runners, which have no guard either).
const noOwnLoopback = 42

func runInOwnLoopback(t *testing.T) bool {
	t.Helper()
	if os.Getenv("PAPERBOAT_TEST_OWN_LOOPBACK") == "1" {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		ifr, err := unix.NewIfreq("lo")
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
			t.Fatal(err)
		}
		ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
		if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
			// A confined namespace (GitHub runners' AppArmor) is created but may not raise loopback: tell the parent.
			os.Exit(noOwnLoopback)
		}
		return false
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^"+t.Name()+"$", "-test.v", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "PAPERBOAT_TEST_OWN_LOOPBACK=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWUSER | unix.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() == noOwnLoopback) {
		t.Logf("no usable unprivileged network namespace here (%v): running on the host loopback", err)
		return false
	}
	if err != nil || !strings.Contains(string(output), "--- PASS: "+t.Name()) {
		t.Fatalf("in its own network namespace: %v\n%s", err, output)
	}
	return true
}

func TestInheritedLeaseKeepsAcceptingAndIsHandedToItsDaemon(t *testing.T) {
	if runInOwnLoopback(t) {
		return
	}
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

func inheritedGatewayFixture(t *testing.T) (*guardServer, int, *os.File) {
	t.Helper()
	listener, err := net.Listen("tcp4", net.JoinHostPort(splitdns.BrowserGatewayIP, "0"))
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	listener.Close() // the old guard exits; the descriptor store keeps the socket
	g := &guardServer{cfg: Config{LoopbackCIDR: "127.100.0.0/16"}, failures: make(chan error, 1), leases: map[string]*guardedLease{}, names: map[controlConn]map[string]string{}, connections: map[controlConn]string{}}
	g.restoreFrom([]*os.File{file}, []string{"pbg|" + strconv.Itoa(port) + "|" + strconv.Itoa(port)})
	return g, port, file
}

// The browser gateway's shared socket survives a guard replacement: its connects queue while no daemon has
// registered a route, its nft rule is kept by a placeholder lease, and the first route starts the accepting.
func TestInheritedGatewayHoldsItsBacklogUntilADaemonRegisters(t *testing.T) {
	if runInOwnLoopback(t) {
		return
	}
	g, port, _ := inheritedGatewayFixture(t)
	gateway := g.gateways[port]
	if gateway == nil || gateway.serving || len(g.leases) != 1 {
		t.Fatalf("restored gateway %v, %d leases", gateway, len(g.leases))
	}
	leases := make([]*guardedLease, 0, 1)
	for _, lease := range g.leases {
		leases = append(leases, lease)
	}
	rules, err := protectionRules(leases)
	if err != nil || !strings.Contains(rules, "ip daddr "+splitdns.BrowserGatewayIP+" tcp dport "+strconv.Itoa(port)+" accept") {
		t.Fatalf("the inherited gateway's rule is missing: %v\n%s", err, rules)
	}
	address := net.JoinHostPort(splitdns.BrowserGatewayIP, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp4", address, time.Second)
	if err != nil {
		t.Fatalf("connect while no daemon has registered: %v", err)
	}
	defer conn.Close()
	daemon := &namesTestConn{id: 1}
	route, err := g.acquireGatewayRoute(t.Context(), daemon, "1000", request{Operation: "acquire", Hostname: splitdns.BrowserGatewayHostname, IP: splitdns.BrowserGatewayIP, Port: port, Handoff: true})
	if err != nil || route == nil {
		t.Fatalf("daemon registration: %v", err)
	}
	if !gateway.serving || len(g.leases) != 1 {
		t.Fatalf("serving=%v, leases %d: the placeholder must give way to the daemon's lease", gateway.serving, len(g.leases))
	}
	for key, lease := range g.leases {
		if lease.owner != controlConn(daemon) || lease.uid != "1000" {
			t.Fatalf("lease %s is %+v", key, lease)
		}
	}
	_ = route.(*gatewayRoute).release()
}

func TestUnclaimedInheritedGatewayIsReleased(t *testing.T) {
	previous := applyPolicy
	applyPolicy = func(context.Context, Config, []*guardedLease) error { return nil }
	t.Cleanup(func() { applyPolicy = previous })
	g, port, _ := inheritedGatewayFixture(t)
	g.releaseUnclaimed()
	if len(g.leases) != 0 || g.gateways[port] != nil {
		t.Fatalf("leases %d, gateway %v after the adoption window", len(g.leases), g.gateways[port])
	}
	if conn, err := net.DialTimeout("tcp4", net.JoinHostPort(splitdns.BrowserGatewayIP, strconv.Itoa(port)), time.Second); err == nil {
		conn.Close()
		t.Fatal("the released gateway still accepts connects")
	}
}

func TestStoreGatewaySendsTheSharedSocketToSystemd(t *testing.T) {
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
	gateway := &sharedGateway{port: 443, listener: listener}
	if err := storeGateway(gateway); err != nil {
		t.Fatal(err)
	}
	buf, oob := make([]byte, 512), make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := server.(*net.UnixConn).ReadMsgUnix(buf, oob)
	if err != nil || oobn == 0 {
		t.Fatalf("datagram: %v (%d control bytes)", err, oobn)
	}
	want := "FDSTORE=1\nFDNAME=pbg|443|" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if string(buf[:n]) != want {
		t.Fatalf("datagram %q, want %q", buf[:n], want)
	}
}
