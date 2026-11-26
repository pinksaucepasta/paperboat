package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
	"github.com/pinksaucepasta/paperboat/cli/internal/testcert"
)

// deadlineGuard answers like the real guard: a call whose context has ended fails (a deadline mid-call drops the
// guard connection), so a stage that inherits another stage's spent budget fails visibly.
type deadlineGuard struct{ *coordinatorNameClient }

func (g deadlineGuard) Acquire(ctx context.Context, hostname string, ip netip.Addr, port int) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return g.coordinatorNameClient.Acquire(ctx, hostname, ip, port)
}

func (g deadlineGuard) ApplyPolicy(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return g.coordinatorNameClient.ApplyPolicy(ctx)
}

func (g deadlineGuard) ReplaceAliases(ctx context.Context, aliases map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return g.coordinatorNameClient.ReplaceAliases(ctx, aliases)
}

// A browser-trust step that runs longer than a whole reconcile budget never withdraws native access: the policy
// and every later guard call (the per-port browser listener) have budgets of their own.
func TestSlowBrowserTrustNeverSpendsALaterStagesBudget(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{
		NameClient: deadlineGuard{names}, ReconcileTimeout: 200 * time.Millisecond,
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") },
		IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
			time.Sleep(400 * time.Millisecond) // twice the reconcile budget
			return ca.TLS(name)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "laptop", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err := c.replaceGuardedRoutes(t.Context(), []*daemonrpc.PeerUpdate{peer}); err != nil {
		t.Fatalf("slow browser trust withdrew native access: %v", err)
	}
	names.mu.Lock()
	applies, native, gatewayPorts := names.applies, names.addresses[8080], append([]int(nil), names.gatewayPorts...)
	names.mu.Unlock()
	if applies == 0 || native == "" || len(c.routes) != 1 || len(c.webRoutes) != 1 {
		t.Fatalf("applies=%d native=%q routes=%d web=%d", applies, native, len(c.routes), len(c.webRoutes))
	}
	if !containsPort(gatewayPorts, 8080) {
		t.Fatalf("the per-port browser listener after the trust step was not acquired: %v", gatewayPorts)
	}
}

func containsPort(ports []int, want int) bool {
	for _, port := range ports {
		if port == want {
			return true
		}
	}
	return false
}

// Stop cancels every reconcile stage before it waits for the coordinator's lock: a stage blocked on the guard (or
// on browser trust) ends at once instead of holding Stop for its whole budget.
func TestStopEndsAReconcileStagePromptly(t *testing.T) {
	entered := make(chan struct{})
	c, err := NewCoordinator(CoordinatorConfig{
		NameClient: deadlineGuard{&coordinatorNameClient{}}, ReconcileTimeout: time.Minute,
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") },
		IssueCertificate: func(ctx context.Context, _, _ string, _ int) (tls.Certificate, error) {
			close(entered)
			<-ctx.Done() // a guard call or trust write that only ends when its stage is cancelled
			return tls.Certificate{}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.lifeCancel, c.running = c.cancel, true
	peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "laptop", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	done := make(chan struct{})
	go func() { _ = c.replaceGuardedRoutes(context.Background(), []*daemonrpc.PeerUpdate{peer}); close(done) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconcile never reached the browser certificate stage")
	}
	started := time.Now()
	_ = c.Stop()
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("Stop took %v with a stage in flight; want it to cancel the stage at once", took)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile did not end after Stop")
	}
}

// Stop waits out a certutil write that outlives the cancellation (it runs to its own bound so the store stays
// whole); cleanup's budget starts after that wait, so it still finishes and the coordinator starts again.
func TestStopAfterAWriteLongerThanCleanupsBudgetStillCleansUp(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	var slow atomic.Bool
	entered := make(chan struct{}, 1)
	c, err := NewCoordinator(CoordinatorConfig{
		SocketAddress: socketAddress(shorttemp.Dir(t)), NameClient: &coordinatorNameClient{},
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") },
		IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
			if slow.Load() {
				entered <- struct{}{}
				time.Sleep(connectTimeout + 500*time.Millisecond) // a write that ignores cancellation
			}
			return ca.TLS(name)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	a := &daemonrpc.PeerUpdate{PeerId: "a", Alias: "laptop", AssignedIp: "127.100.0.24", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	b := &daemonrpc.PeerUpdate{PeerId: "b", Alias: "desktop", AssignedIp: "127.100.0.25", Approved: true, Online: true, ExportedPorts: []int32{8081}}
	if err = c.replaceGuardedRoutes(t.Context(), []*daemonrpc.PeerUpdate{a}); err != nil {
		t.Fatal(err)
	}
	slow.Store(true)
	go func() { _ = c.replaceGuardedRoutes(context.Background(), []*daemonrpc.PeerUpdate{a, b}) }()
	<-entered
	if err = c.Stop(); err != nil {
		t.Fatalf("Stop after a long write: %v", err)
	}
	c.mu.Lock()
	left := len(c.cleanups)
	c.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d cleanups left after Stop; the next Start would be refused", left)
	}
	slow.Store(false)
	if err = c.Start(t.Context()); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	_ = c.Stop()
}

// A transient guard failure in a later stage is retried even when an earlier stage ran out its own budget: only
// Stop's cancellation rules a retry out.
func TestALaterStagesTransientFailureIsRetriedAfterASlowEarlierStage(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	dials := 0
	c, err := NewCoordinator(CoordinatorConfig{
		SocketAddress: socketAddress(shorttemp.Dir(t)), ReconcileTimeout: 200 * time.Millisecond,
		ConnectNameClient: func(context.Context) (guardedNameClient, error) {
			dials++
			client := &coordinatorNameClient{}
			if dials == 1 {
				client.failApply = os.ErrDeadlineExceeded // the policy apply stage times out once
			}
			return client, nil
		},
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") },
		IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
			time.Sleep(400 * time.Millisecond) // the first stage outlives its budget
			return ca.TLS(name)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "laptop", AssignedIp: "127.100.0.26", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err = c.replaceGuardedRoutes(t.Context(), []*daemonrpc.PeerUpdate{peer}); err != nil || dials != 2 {
		t.Fatalf("err=%v dials=%d; want the policy apply retried on a fresh guard connection", err, dials)
	}
}

// Several new browser routes share one stage budget: certificate calls that block never hold the reconcile (and
// c.mu) for one budget per route; the routes left over stay pending and the retry issues them.
func TestBrowserCertificatesShareOneStageBudget(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	var blocking atomic.Bool
	blocking.Store(true)
	c, err := NewCoordinator(CoordinatorConfig{
		RetryBase: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond, ReconcileTimeout: 300 * time.Millisecond,
		NameClient:  &coordinatorNameClient{},
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") },
		IssueCertificate: func(ctx context.Context, name, _ string, _ int) (tls.Certificate, error) {
			if blocking.Load() {
				select { // a certificate call that ends only with its budget (capped so a regression fails, not hangs)
				case <-ctx.Done():
					return tls.Certificate{}, ctx.Err()
				case <-time.After(3 * time.Second):
				}
			}
			return ca.TLS(name)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.ctx, c.running = t.Context(), true
	c.mu.Unlock()
	t.Cleanup(c.resetReconcileRetry)
	var peers []*daemonrpc.PeerUpdate
	for i, name := range []string{"one", "two", "three"} {
		peers = append(peers, &daemonrpc.PeerUpdate{PeerId: name, Alias: name, AssignedIp: fmt.Sprintf("127.100.0.%d", 30+i), Approved: true, Online: true, ExportedPorts: []int32{8080}})
	}
	started := time.Now()
	if err := c.reconcileSnapshot(t.Context(), peers, 1); err != nil {
		t.Fatalf("blocked browser certificates must keep native access: %v", err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("reconcile took %v with 3 blocked certificate calls; want one shared stage budget", took)
	}
	blocking.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		pending, web := c.browserPending, len(c.webRoutes)
		c.mu.Unlock()
		if !pending && web == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retry did not finish the browser routes: pending=%v webRoutes=%d", pending, web)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
