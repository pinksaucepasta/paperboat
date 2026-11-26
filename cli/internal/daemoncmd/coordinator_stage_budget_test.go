package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
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
