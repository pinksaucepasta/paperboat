//go:build linux

package deviceguard

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepeatedNamesSnapshotDoesNotReconfigureResolver(t *testing.T) {
	directory := t.TempDir()
	counter := filepath.Join(directory, "calls")
	// A private command fixture counts OS projection without changing host DNS.
	if err := os.WriteFile(filepath.Join(directory, "resolvectl"), []byte("#!/bin/sh\nprintf 'called\\n' >> \"$PAPERBOAT_RESOLVER_CALLS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("PAPERBOAT_RESOLVER_CALLS", counter)
	owner := &certificateConnection{identity: "owner"}
	g := &guardServer{cfg: Config{ConfigureResolver: true}, leases: map[string]*guardedLease{"a": {hostname: "office.pprbt", ip: "127.100.23.45", owner: owner}, "b": {hostname: "studio.pprbt", ip: "127.100.23.46", owner: owner}}, names: map[controlConn]map[string]string{}, dnsView: map[string]string{}}
	publish := func(names map[string]string) {
		t.Helper()
		if _, err := g.handle(context.Background(), owner, "owner", request{Operation: "names", Names: names}); err != nil {
			t.Fatal(err)
		}
	}
	publish(map[string]string{"office.pprbt": "127.100.23.45"})
	publish(map[string]string{"office.pprbt": "127.100.23.45"})
	calls, err := os.ReadFile(counter)
	if err != nil || string(calls) != "called\n" {
		t.Fatalf("repeated snapshot reconfigured resolver: %q %v", calls, err)
	}
	// Actual names changing under the same suffix must still invalidate negative caches.
	publish(map[string]string{"studio.pprbt": "127.100.23.46"})
	calls, err = os.ReadFile(counter)
	if err != nil || string(calls) != "called\ncalled\n" {
		t.Fatalf("changed names did not reconfigure resolver: %q %v", calls, err)
	}
	delete(g.leases, "b")
	if _, err = g.handle(context.Background(), owner, "owner", request{Operation: "names", Names: map[string]string{"studio.pprbt": "127.100.23.46"}}); err == nil {
		t.Fatal("unchanged snapshot bypassed current listener ownership")
	}
}

func TestNamesRetirementProjectsResolver(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root only to retire an isolated test socket mark")
	}
	directory := t.TempDir()
	counter := filepath.Join(directory, "calls")
	for name, script := range map[string]string{
		"resolvectl": "#!/bin/sh\nprintf 'called\\n' >> \"$PAPERBOAT_RESOLVER_CALLS\"\nif [ \"$PAPERBOAT_RESOLVER_FAIL\" = 1 ]; then exit 9; fi\n",
		"nft":        "#!/bin/sh\nif [ \"$1\" = '-f' ]; then while IFS= read -r line; do :; done; fi\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
	t.Setenv("PAPERBOAT_RESOLVER_CALLS", counter)
	t.Setenv("PAPERBOAT_RESOLVER_FAIL", "")
	owner := &certificateConnection{identity: "owner"}
	g := &guardServer{cfg: Config{ConfigureResolver: true}, leases: map[string]*guardedLease{}, names: map[controlConn]map[string]string{}, dnsView: map[string]string{}, failures: make(chan error, 1)}
	install := func() int {
		t.Helper()
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		port := listener.Addr().(*net.TCPAddr).Port
		g.leases[net.JoinHostPort("127.100.23.45", strconv.Itoa(port))] = &guardedLease{hostname: "office.pprbt", ip: "127.100.23.45", port: port, uid: "0", owner: owner, listener: listener}
		if _, err = g.handle(context.Background(), owner, "owner", request{Operation: "names", Names: map[string]string{"office.pprbt": "127.100.23.45"}}); err != nil {
			t.Fatal(err)
		}
		return port
	}
	checkCalls := func(want int) {
		t.Helper()
		data, err := os.ReadFile(counter)
		if err != nil || strings.Count(string(data), "called\n") != want {
			t.Fatalf("resolver projection calls=%q error=%v want=%d", data, err, want)
		}
	}
	port := install()
	checkCalls(1)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.handle(canceled, owner, "owner", request{Operation: "release", IP: "127.100.23.45", Port: port}); err != nil {
		t.Fatal(err)
	}
	checkCalls(2)
	if _, err := g.handle(context.Background(), owner, "owner", request{Operation: "names", Names: nil}); err != nil {
		t.Fatal(err)
	}
	checkCalls(2)
	install()
	checkCalls(3)
	g.withdraw(canceled, owner)
	checkCalls(4)
	g.withdraw(canceled, &certificateConnection{identity: "readiness"})
	checkCalls(4)
	install()
	checkCalls(5)
	t.Setenv("PAPERBOAT_RESOLVER_FAIL", "1")
	g.withdraw(canceled, owner)
	select {
	case err := <-g.failures:
		if err == nil {
			t.Fatal("cleanup error lost")
		}
	default:
		t.Fatal("failed resolver cleanup did not fail guard")
	}
}
