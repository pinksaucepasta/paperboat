//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
)

// guardCallBound is far below operationTimeout (9 s) and far above every measured call (under 0.2 s at the ceiling).
const guardCallBound = 3 * time.Second

func limitAddress(i int) string { return fmt.Sprintf("127.100.%d.%d", 1+i/250, 1+i%250) }
func limitHost(i int) string    { return fmt.Sprintf("machine-%d.%s", i, testBrowserDomain) }

// limitGuard is a guard whose one connection holds the gateway and a listener for each of n machines.
func limitGuard(t *testing.T, n int) (*guardServer, *testControlConn) {
	t.Helper()
	previous := applyPolicy
	applyPolicy = func(context.Context, Config, []*guardedLease) error { return nil }
	t.Cleanup(func() { applyPolicy = previous })
	conn := &testControlConn{identity: "1001"}
	g := &guardServer{
		cfg:         Config{StateDir: testStateDir(t), LoopbackCIDR: "127.100.0.0/16"},
		reserved:    reservations{Names: map[string]string{}, Claims: map[string]claim{}},
		leases:      map[string]*guardedLease{"gateway": {hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: 443, uid: "1001", owner: conn}},
		names:       map[controlConn]map[string]string{},
		connections: map[controlConn]string{conn: "1001"},
	}
	g.claims = &claimBook{Claims: g.reserved.Claims}
	for i := range n {
		g.leases[limitAddress(i)+":22"] = &guardedLease{hostname: limitHost(i), ip: limitAddress(i), port: 22, uid: "1001", owner: conn}
	}
	return g, conn
}

func TestGuardServesEveryMachineUpToTheCeiling(t *testing.T) {
	for _, n := range []int{129, 179, 512, 513, 1000, MaxRoutedMachines} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			g, conn := limitGuard(t, n)
			names, aliases, machines := map[string]string{}, map[string]string{}, []string{}
			for i := range n {
				names[limitHost(i)] = limitAddress(i)
				for _, port := range []string{"22.", "80.", "8080."} {
					aliases[port+limitHost(i)] = splitdns.BrowserGatewayHostname
				}
				aliases["*."+limitHost(i)] = "80." + limitHost(i)
				machines = append(machines, fmt.Sprintf("machine_%d", i))
			}
			timed := func(step string, run func() error) {
				t.Helper()
				started := time.Now()
				if err := run(); err != nil {
					t.Fatalf("%s: %v", step, err)
				}
				took := time.Since(started)
				t.Logf("%s: %s", step, took.Round(time.Microsecond))
				if took > guardCallBound {
					t.Fatalf("%s took %s, bound %s", step, took, guardCallBound)
				}
			}
			call := func(in request) error { _, err := g.handle(t.Context(), conn, "1001", in); return err }
			timed("claims of every machine", func() error {
				for i, machine := range machines {
					in := request{Operation: "acquire", Account: "acct", Machine: machine, Hostname: limitHost(i), IP: limitAddress(i), Port: 22}
					if _, err := g.assignClaim("1001", in); err != nil {
						return err
					}
					if err := g.reserveName("1001", limitHost(i)); err != nil {
						return err
					}
				}
				return nil
			})
			timed("apply", func() error { return call(request{Operation: "apply"}) })
			timed("aliases", func() error { return call(request{Operation: "aliases", Names: aliases}) })
			timed("names", func() error { return call(request{Operation: "names", Names: names}) })
			timed("claims", func() error { return call(request{Operation: "claims", Account: "acct", Machines: machines}) })
			if len(g.names[conn]) != n || len(g.aliases[conn]) != 4*n || len(g.claims.addresses("1001")) != n {
				t.Fatalf("names=%d aliases=%d claims=%d, want %d machines", len(g.names[conn]), len(g.aliases[conn]), len(g.claims.addresses("1001")), n)
			}
			g.certificates = map[controlConn]map[string]cachedCertificate{conn: {}}
			for i := range n {
				g.certificates[conn][limitHost(i)] = cachedCertificate{}
			}
			timed("certificate authorization of every machine", func() error {
				for i := range n {
					if !g.certificateAuthorizedDomain(conn, "1001", fmt.Sprintf("machine-%d", i), testBrowserDomain) {
						return errors.New("refused")
					}
				}
				return nil
			})
			timed("certificate pruning", func() error { g.pruneCertificates(conn, "1001"); return nil })
			if len(g.certificates[conn]) != n {
				t.Fatalf("pruning dropped live certificates: %d of %d left", len(g.certificates[conn]), n)
			}
			for _, in := range []request{
				{Operation: "names", Names: names}, {Operation: "aliases", Names: aliases}, {Operation: "claims", Account: "acct", Machines: machines},
			} {
				if data, err := json.Marshal(in); err != nil || checkFrame(data) != nil {
					t.Fatalf("%s request: %d bytes, %v", in.Operation, len(data), err)
				}
			}
		})
	}
}

func TestGuardRefusesMoreThanTheCeilingAndKeepsWhatItServed(t *testing.T) {
	g, conn := limitGuard(t, 1)
	call := func(in request) error { _, err := g.handle(t.Context(), conn, "1001", in); return err }
	if err := call(request{Operation: "names", Names: map[string]string{limitHost(0): limitAddress(0)}}); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for i := range MaxRoutedMachines + 1 {
		names[limitHost(i)] = limitAddress(i)
	}
	if err := call(request{Operation: "names", Names: names}); err == nil {
		t.Fatal("more names than the ceiling accepted")
	}
	aliases := map[string]string{}
	for i := range MaxBrowserAliases + 1 {
		aliases["80."+limitHost(i)] = splitdns.BrowserGatewayHostname
	}
	if err := call(request{Operation: "aliases", Names: aliases}); err == nil {
		t.Fatal("more aliases than the ceiling accepted")
	}
	if err := call(request{Operation: "claims", Account: "acct", Machines: make([]string, maxClaimMachines+1)}); err == nil {
		t.Fatal("more claim machines than the ceiling accepted")
	}
	if len(g.names[conn]) != 1 || len(g.aliases[conn]) != 0 || len(g.reserved.Names) != 0 {
		t.Fatalf("a refused request changed state: %d names %d aliases %d reservations", len(g.names[conn]), len(g.aliases[conn]), len(g.reserved.Names))
	}
	delete(names, limitHost(MaxRoutedMachines)) // the rest have no listener: the refusal stays short
	if err := call(request{Operation: "names", Names: names}); !errors.Is(err, ErrNamesRefused) || len(err.Error()) > 4096 {
		t.Fatalf("names refusal: %.200v", err)
	}
}

func TestGuardQuotasBoundOneUserAndLeaveRoomForOthers(t *testing.T) {
	g, _ := limitGuard(t, 0)
	for i := range maxUserNames {
		if err := g.reserveName("1001", fmt.Sprintf("n%d.%s", i, testBrowserDomain)); err != nil {
			t.Fatal(i, err)
		}
	}
	// The cap prunes names nothing serves; these are served.
	live := map[string]string{}
	for i := range maxUserNames {
		live[fmt.Sprintf("n%d.%s", i, testBrowserDomain)] = "x"
	}
	for conn := range g.connections {
		g.names[conn] = live
	}
	if err := g.reserveName("1001", "over."+testBrowserDomain); !errors.Is(err, ErrListenerRefused) {
		t.Fatalf("name over the user quota: %v", err)
	}
	if err := g.reserveName("1002", "over."+testBrowserDomain); err != nil {
		t.Fatalf("another user is refused: %v", err)
	}
	now := time.Now()
	for i := range maxUserClaims {
		if _, err := g.claims.assign(claimKey{UID: "1001", Account: "a", Machine: fmt.Sprint(i)}, netip.MustParseAddr("127.100.9.9"), now); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := g.claims.assign(claimKey{UID: "1001", Account: "a", Machine: "over"}, netip.MustParseAddr("127.100.9.9"), now); !errors.Is(err, ErrListenerRefused) {
		t.Fatalf("claim over the user quota: %v", err)
	}
	if _, err := g.claims.assign(claimKey{UID: "1002", Account: "a", Machine: "m"}, netip.MustParseAddr("127.100.9.9"), now); err != nil {
		t.Fatalf("another user is refused: %v", err)
	}
	for i := range maxUserListeners {
		g.leases[fmt.Sprint(i)] = &guardedLease{uid: "1001"}
	}
	if !g.listenersFull("1001") || g.listenersFull("1002") {
		t.Fatal("the listener quota must bound one user and leave room for others")
	}
	for _, id := range []string{"", strings.Repeat("a", maxClaimField+1), "a\x00b", "a\"b", "a<b", "é"} {
		in := request{Operation: "acquire", Account: "acct", Machine: id, Hostname: limitHost(0), IP: "127.100.0.7", Port: 22}
		if _, err := g.assignClaim("1003", in); !errors.Is(err, ErrListenerRefused) {
			t.Fatalf("machine id %q: %v", id, err)
		}
	}
}

// Whatever users fill in, the guard can read its own journal back at its next start.
func TestGuardJournalAtEveryLimitStaysReadable(t *testing.T) {
	sid := "S-1-5-21-4294967295-4294967295-4294967295-4294967295"
	size := func(v any) int {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return len(data)
	}
	id := strings.Repeat("a", maxClaimField)
	name := size(map[string]string{nameKey(sid, strings.Repeat("h", 253)): sid})
	entry := size(map[string]claim{claimKey{UID: sid, Account: id, Machine: id}.String(): {IP: "127.100.254.254", HeldUntil: time.Now()}})
	if total := name*maxHostNames + entry*maxHostClaims; total > maxJournal {
		t.Fatalf("a full journal is %d bytes, the guard loads %d", total, maxJournal)
	}
}

func TestGuardFrameBounds(t *testing.T) {
	if err := checkFrame(make([]byte, maxFrame+1)); !errors.Is(err, errFrameSize) {
		t.Fatal(err)
	}
	if _, err := readFrameBody(strings.NewReader("x"), maxFrame+1); !errors.Is(err, errFrameSize) {
		t.Fatal(err)
	}
	if data, err := readFrameBody(strings.NewReader(strings.Repeat("x", maxFrame)), maxFrame); err != nil || len(data) != maxFrame {
		t.Fatal(len(data), err)
	}
	if _, err := readFrameBody(strings.NewReader("short"), 6); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	// A header that announces the largest frame and sends nothing must not reserve that memory.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 100 {
		_, _ = readFrameBody(strings.NewReader(""), maxFrame)
	}
	runtime.ReadMemStats(&after)
	if grown := after.TotalAlloc - before.TotalAlloc; grown > maxFrame {
		t.Fatalf("100 empty frames allocated %d bytes", grown)
	}
}

// A request above the frame limit is refused before a byte is written, so the connection and its leases survive.
func TestOversizedRequestKeepsTheConnection(t *testing.T) {
	client, done := oversizeClient(t)
	defer done()
	names := map[string]netip.Addr{}
	for i := range 4 {
		names[strings.Repeat("a", maxFrame/4)+fmt.Sprint(i)] = netip.MustParseAddr("127.100.0.1")
	}
	if err := client.ReplaceNames(t.Context(), names); !errors.Is(err, errFrameSize) {
		t.Fatal(err)
	}
	if client.conn == nil {
		t.Fatal("the connection was dropped")
	}
}

func TestFrameHeaderAloneIsRefusedAboveTheLimit(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 0xffffffff) // the same on either byte order
	server, peer := net.Pipe()
	defer server.Close()
	go func() { _, _ = peer.Write(header[:]); _ = peer.Close() }()
	if err := receiveFrame(server); !errors.Is(err, errFrameSize) {
		t.Fatal(err)
	}
}

func TestStoredLeaseNamesFitOneEnvironmentString(t *testing.T) {
	g, conn := limitGuard(t, 0)
	total := 0
	for i := range MaxMachineListeners {
		lease := &guardedLease{uid: "1001", hostname: limitHost(i), ip: limitAddress(i), port: 22, owner: conn}
		if size := g.storedNameSize(lease); size > 0 {
			lease.storedName = size
			g.storedNames += size
			total += len(leaseFDName(lease)) + 1
		}
	}
	if total > maxStoredLeaseNames {
		t.Fatalf("%d bytes of descriptor names, limit %d", total, maxStoredLeaseNames)
	}
}

func TestReleaseAllWithdrawsOnlyTheCallersListenersInOnePolicyChange(t *testing.T) {
	wantApplies := 1
	if releaseDefersPolicy {
		wantApplies = 0
	}
	g, conn := limitGuard(t, 1000)
	applies := 0
	applyPolicy = func(context.Context, Config, []*guardedLease) error { applies++; return nil }
	shut := 0
	previous := shutdownLease
	shutdownLease = func(net.Listener) error { shut++; return nil } // the real one needs root
	t.Cleanup(func() { shutdownLease = previous })
	other := &testControlConn{identity: "1002"}
	g.leases["127.100.200.1:22"] = &guardedLease{hostname: "theirs", ip: "127.100.200.1", port: 22, uid: "1002", owner: other}
	keys := []string{"127.100.200.1:22", "127.100.201.1:22"}
	for key, lease := range g.leases {
		if lease.port == 22 && lease.owner == conn {
			keys = append(keys, key)
		}
	}
	if _, err := g.handle(t.Context(), conn, "1001", request{Operation: "release_all", Listeners: keys}); err != nil {
		t.Fatal(err)
	}
	if applies != wantApplies || shut != 1000 || len(g.leases) != 2 || g.leases["127.100.200.1:22"] == nil {
		t.Fatalf("applies=%d shut=%d leases=%d", applies, shut, len(g.leases))
	}
	if _, err := g.handle(t.Context(), conn, "1001", request{Operation: "release_all", Listeners: make([]string, maxUserListeners+1)}); err == nil {
		t.Fatal("more listeners than the ceiling accepted")
	}
}
