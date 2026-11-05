package daemoncmd

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
)

// claimingNameClient assigns addresses per machine id like the guard: an id keeps its claim, and a held
// claim's address is never given to another id, so a re-paired machine gets a new address.
type claimingNameClient struct {
	*coordinatorNameClient
	claims map[string]netip.Addr
	next   netip.Addr
}

func (c *claimingNameClient) AcquireMachine(ctx context.Context, _, machineID, hostname string, _ netip.Addr, port int) (net.Listener, netip.Addr, error) {
	assigned, ok := c.claims[machineID]
	if !ok {
		assigned, c.next = c.next, c.next.Next()
		c.claims[machineID] = assigned
	}
	listener, err := c.Acquire(ctx, hostname, assigned, port)
	return listener, assigned, err
}

// A machine re-paired under the same name gets a new id; the daemon must resolve it to the address the guard
// bound its listener on, not the snapshot's address (still held for the old id), or connects are refused.
func TestCoordinatorResolvesAMachineWithANewIDToTheAddressItsListenerHolds(t *testing.T) {
	names := &claimingNameClient{coordinatorNameClient: &coordinatorNameClient{}, claims: map[string]netip.Addr{}, next: netip.MustParseAddr("127.100.0.7")}
	c, err := NewCoordinator(CoordinatorConfig{
		ConnectNameClient: func(context.Context) (guardedNameClient, error) { return names, nil },
		DialMachine:       func(context.Context, string, int) (net.Conn, error) { return nil, net.ErrClosed },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx, c.cancel = context.WithCancel(t.Context())
	defer c.Stop()
	peer := func(id string) []*daemonrpc.PeerUpdate {
		return []*daemonrpc.PeerUpdate{{PeerId: id, Alias: "laner-s09h", AssignedIp: "127.100.0.7", Approved: true, Online: true, ExportedPorts: []int32{8080}}}
	}
	if err := c.reconcileSnapshot(t.Context(), peer("machine_old"), 1); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileSnapshot(t.Context(), peer("machine_new"), 2); err != nil {
		t.Fatal(err)
	}
	got, err := c.rpcBackend.ResolveMachine(t.Context(), "machine_new")
	if err != nil {
		t.Fatal(err)
	}
	want := names.claims["machine_new"]
	if got.GetAssignedIp() != want.String() || len(got.GetForwardedPorts()) != 1 {
		t.Fatalf("new machine id resolves to %s ports %v; its listener holds %s", got.GetAssignedIp(), got.GetForwardedPorts(), want)
	}
	if _, err := c.rpcBackend.ResolveMachine(t.Context(), "machine_old"); err == nil {
		t.Fatal("the removed machine id still resolves")
	}
}
