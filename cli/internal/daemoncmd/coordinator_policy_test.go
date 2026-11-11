package daemoncmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
)

func manyPeers(n int) []*daemonrpc.PeerUpdate {
	peers := make([]*daemonrpc.PeerUpdate, 0, n)
	for i := range n {
		peers = append(peers, &daemonrpc.PeerUpdate{PeerId: fmt.Sprintf("peer-%d", i), Alias: fmt.Sprintf("m%d", i), AssignedIp: fmt.Sprintf("127.100.1.%d", i+2), Approved: true, Online: true, ExportedPorts: []int32{22, 8080}})
	}
	return peers
}

// victus, 2026-11-11: with ~20 peers the guard applied the whole WFP policy once per acquired listener and the
// reconcile never finished inside its deadline, so no native listener ever came up. One reconcile now asks the
// guard for one policy transaction, however many listeners it acquired.
func TestCoordinatorAppliesTheGuardPolicyOncePerReconcile(t *testing.T) {
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	if err := c.replaceGuardedRoutes(t.Context(), manyPeers(20)); err != nil {
		t.Fatal(err)
	}
	if names.acquires != 40 || names.applies != 1 || len(c.routes) != 40 || len(names.names) != 20 {
		t.Fatalf("acquires=%d applies=%d routes=%d names=%d, want 40 acquires, 1 apply, 40 routes, 20 names", names.acquires, names.applies, len(c.routes), len(names.names))
	}
}

// A failed policy transaction leaves the guard's previous policy in place (the new addresses stay denied) and the
// daemon keeps none of the listeners it cannot make reachable: it withdraws and retries on the next reconcile.
func TestCoordinatorFailedGuardPolicyWithdrawsTheNewListeners(t *testing.T) {
	names := &coordinatorNameClient{failApply: errors.New("apply guard policy: transaction aborted")}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	err = c.replaceGuardedRoutes(t.Context(), manyPeers(3))
	var operation coordinatorOperationError
	if !errors.As(err, &operation) || operation.operation != "native_listener" {
		t.Fatalf("err=%v, want the native_listener step", err)
	}
	if len(c.routes) != 0 || len(names.names) != 0 || c.nameClient != nil {
		t.Fatalf("routes=%d names=%d client kept=%v after a failed policy", len(c.routes), len(names.names), c.nameClient != nil)
	}
}
