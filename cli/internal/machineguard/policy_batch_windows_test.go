//go:build windows

package machineguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
)

const policyTestUID = "S-1-5-21-1-2-3-1001"

// policyRecorder stands in for WFP: installed is the last committed policy, a failed or blocked transaction
// leaves it as it was (runTransaction aborts).
type policyRecorder struct {
	mu        sync.Mutex
	calls     int
	failure   error
	block     chan struct{} // when set, a transaction waits for it to close
	installed []string
}

func (r *policyRecorder) apply(_ context.Context, _ Config, leases []*guardedLease) error {
	r.mu.Lock()
	r.calls++
	block, failure := r.block, r.failure
	r.mu.Unlock()
	if block != nil {
		<-block
	}
	if failure != nil {
		return failure
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installed = r.installed[:0]
	for _, lease := range leases {
		r.installed = append(r.installed, net.JoinHostPort(lease.ip, fmt.Sprint(lease.port)))
	}
	return nil
}

func (r *policyRecorder) snapshot() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, slices.Clone(r.installed)
}

func newPolicyTestGuard(t *testing.T) (*guardServer, *testControlConn, *policyRecorder) {
	t.Helper()
	recorder := &policyRecorder{}
	previous := applyPolicy
	applyPolicy = recorder.apply
	t.Cleanup(func() { applyPolicy = previous })
	conn := &testControlConn{identity: policyTestUID}
	g := &guardServer{
		cfg:         Config{StateDir: testStateDir(t), LoopbackCIDR: "127.100.0.0/16"},
		reserved:    reservations{Names: map[string]string{}, Claims: map[string]claim{}},
		leases:      map[string]*guardedLease{},
		names:       map[controlConn]map[string]string{},
		connections: map[controlConn]string{conn: policyTestUID},
		failures:    make(chan error, 1),
	}
	g.claims = &claimBook{Claims: g.reserved.Claims}
	return g, conn, recorder
}

// call runs one request the way serveConnection does: under the guard lock.
func (g *guardServer) call(t *testing.T, conn controlConn, in request) (net.Listener, error) {
	t.Helper()
	return g.callAs(t, conn, policyTestUID, in)
}

func (g *guardServer) callAs(t *testing.T, conn controlConn, uid string, in request) (net.Listener, error) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	in, err := g.assignClaim(uid, in)
	if err != nil {
		return nil, err
	}
	return g.handle(t.Context(), conn, uid, in)
}

func machineRequest(i int) request {
	return request{Operation: "acquire", Account: "acct", Machine: fmt.Sprintf("m%d", i), Hostname: fmt.Sprintf("m%d.%s", i, testBrowserDomain), IP: fmt.Sprintf("127.100.1.%d", i+2), Port: 41000 + i}
}

func acquireMachine(t *testing.T, g *guardServer, conn controlConn, i int) {
	t.Helper()
	listener, err := g.call(t, conn, machineRequest(i))
	if err != nil {
		t.Fatalf("acquire %d: %v", i, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
}

// Windows test machine, 2026-11-11: every acquire ran a full WFP Apply (enumerate every system filter, delete and re-add the
// Paperboat ones) under the guard lock, one peer after another, so ~20 peers never finished inside the daemon's
// 15 s reconcile and no native listener came up. Acquires now only bind; the daemon's one "apply" per reconcile
// installs the policy for all of them. A failed apply keeps the previous policy (the new addresses stay denied)
// and the next apply retries it.
func TestGuardAppliesThePolicyOnceForManyAcquiresAndKeepsThePreviousOneOnFailure(t *testing.T) {
	g, conn, recorder := newPolicyTestGuard(t)
	apply := func() error {
		_, err := g.call(t, conn, request{Operation: "apply"})
		return err
	}
	for i := range 20 {
		acquireMachine(t, g, conn, i)
	}
	if calls, _ := recorder.snapshot(); calls != 0 {
		t.Fatalf("acquires ran %d policy transactions, want 0", calls)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if calls, installed := recorder.snapshot(); calls != 1 || len(installed) != 20 {
		t.Fatalf("apply: calls=%d installed=%d, want one transaction with 20 leases", calls, len(installed))
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if calls, _ := recorder.snapshot(); calls != 1 {
		t.Fatalf("an apply with nothing new ran a transaction: calls=%d", calls)
	}
	acquireMachine(t, g, conn, 20)
	recorder.failure = errors.New("WFP transaction aborted")
	if err := apply(); !errors.Is(err, recorder.failure) {
		t.Fatalf("failed apply: err=%v", err)
	}
	if _, installed := recorder.snapshot(); len(installed) != 20 || slices.Contains(installed, "127.100.1.22:41020") {
		t.Fatalf("a failed apply changed the installed policy: %v", installed)
	}
	recorder.failure = nil
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if _, installed := recorder.snapshot(); len(installed) != 21 {
		t.Fatalf("retry installed %d leases, want 21", len(installed))
	}
}

// However slow a policy transaction is, concurrent acquires never run one: each returns without waiting for WFP.
func TestGuardConcurrentAcquiresNeverWaitForAPolicyTransaction(t *testing.T) {
	g, conn, recorder := newPolicyTestGuard(t)
	recorder.block = make(chan struct{})
	defer close(recorder.block)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Go(func() {
			listener, err := g.call(t, conn, machineRequest(i))
			if err == nil {
				t.Cleanup(func() { _ = listener.Close() })
			}
			errs <- err
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // the daemon's per-call pipe deadline
		t.Fatal("concurrent acquires waited on the blocked policy transaction")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls, _ := recorder.snapshot(); calls != 0 || len(g.leases) != 20 {
		t.Fatalf("calls=%d leases=%d, want 0 transactions and 20 leases", calls, len(g.leases))
	}
}

// A release shuts the guard's socket at once and leaves the WFP transaction to the next apply: the rule left
// behind only lets the same OS user connect to its own claimed address, where nothing listens any more.
func TestGuardReleaseDefersPolicy(t *testing.T) {
	g, conn, recorder := newPolicyTestGuard(t)
	listener, err := g.call(t, conn, machineRequest(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	acquireMachine(t, g, conn, 1)
	if _, err := g.call(t, conn, request{Operation: "apply"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.call(t, conn, request{Operation: "release", IP: "127.100.1.2", Port: 41000}); err != nil {
		t.Fatal(err)
	}
	if calls, _ := recorder.snapshot(); calls != 1 {
		t.Fatalf("release ran a policy transaction: calls=%d, want 1 (the apply)", calls)
	}
	if dialed, err := net.DialTimeout("tcp", "127.100.1.2:41000", time.Second); err == nil {
		dialed.Close()
		t.Fatal("the released listener still accepts connections")
	}
	if _, err := g.call(t, conn, request{Operation: "apply"}); err != nil {
		t.Fatal(err)
	}
	if calls, installed := recorder.snapshot(); calls != 2 || slices.Contains(installed, "127.100.1.2:41000") || len(installed) != 1 {
		t.Fatalf("apply after release: calls=%d installed=%v, want the released lease gone", calls, installed)
	}
}

// Windows test machine, 2026-11-16: three OS users' daemons share one guard. When one daemon's routes went away, each release
// ran a full WFP transaction under the guard lock; the other users' guard calls waited past their 5 s pipe deadline
// and their local access went degraded (deadline_exceeded) until the daemon was restarted.
func TestGuardReleaseSparesOtherUsers(t *testing.T) {
	g, alice, recorder := newPolicyTestGuard(t)
	const bobUID = "S-1-5-21-1-2-3-1002"
	bob := &testControlConn{identity: bobUID}
	g.connections[bob] = bobUID
	for i := range 20 {
		acquireMachine(t, g, alice, i)
	}
	if _, err := g.call(t, alice, request{Operation: "apply"}); err != nil {
		t.Fatal(err)
	}
	recorder.block = make(chan struct{}) // WFP takes as long as it likes
	defer close(recorder.block)
	done := make(chan error, 2)
	go func() {
		for i := range 20 {
			in := machineRequest(i)
			if _, err := g.call(t, alice, request{Operation: "release", IP: in.IP, Port: in.Port}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	go func() {
		in := request{Operation: "acquire", Account: "acct-b", Machine: "b0", Hostname: "b0." + testBrowserDomain, IP: "127.100.2.2", Port: 42000}
		listener, err := g.callAs(t, bob, bobUID, in)
		if err == nil {
			t.Cleanup(func() { _ = listener.Close() })
		}
		done <- err
	}()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second): // the daemon's per-call pipe deadline
			t.Fatal("one user's releases held another user's guard call past its deadline")
		}
	}
}

// A browser gateway route is a lease like a machine listener: denied until the apply that follows it.
func TestGuardGatewayRouteStaysDeniedUntilApply(t *testing.T) {
	g, conn, recorder := newPolicyTestGuard(t)
	route, err := g.call(t, conn, request{Operation: "acquire", Hostname: splitdns.BrowserGatewayHostname, IP: splitdns.BrowserGatewayIP, Port: 47123, Handoff: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		_ = shutdownListener(route)
	})
	gateway := net.JoinHostPort(splitdns.BrowserGatewayIP, "47123")
	if calls, installed := recorder.snapshot(); calls != 0 || slices.Contains(installed, gateway) {
		t.Fatalf("gateway route installed before apply: calls=%d installed=%v", calls, installed)
	}
	if _, err := g.call(t, conn, request{Operation: "apply"}); err != nil {
		t.Fatal(err)
	}
	if _, installed := recorder.snapshot(); !slices.Contains(installed, gateway) {
		t.Fatalf("apply did not install the gateway route: %v", installed)
	}
}
