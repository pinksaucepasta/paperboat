package daemoncmd

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
)

// resetForwardFixture is a coordinator with one forwarded connection open through a guarded route.
type resetForwardFixture struct {
	c       *Coordinator
	clients []*coordinatorNameClient
	peers   []*daemonrpc.PeerUpdate
	local   net.Conn
	remote  net.Conn
}

func newResetForwardFixture(t *testing.T) *resetForwardFixture {
	t.Helper()
	f := &resetForwardFixture{clients: []*coordinatorNameClient{{}, {}, {}}}
	opened := make(chan net.Conn, 1)
	next := 0
	c, err := NewCoordinator(CoordinatorConfig{
		ConnectNameClient: func(context.Context) (guardedNameClient, error) { client := f.clients[next]; next++; return client, nil },
		DialMachine: func(context.Context, string, int) (net.Conn, error) {
			client, remote := net.Pipe()
			opened <- remote
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx, c.cancel = context.WithCancel(t.Context())
	f.c = c
	f.peers = []*daemonrpc.PeerUpdate{{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}}
	if err := c.replaceGuardedRoutes(t.Context(), f.peers); err != nil {
		t.Fatal(err)
	}
	f.clients[0].mu.Lock()
	address := f.clients[0].addresses[8080]
	f.clients[0].mu.Unlock()
	if f.local, err = net.Dial("tcp", address); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.local.Close() })
	f.remote = <-opened
	t.Cleanup(func() { _ = f.remote.Close() })
	f.echo(t, "a")
	return f
}

// echo sends one byte through the forward and fails unless the machine side receives it.
func (f *resetForwardFixture) echo(t *testing.T, b string) {
	t.Helper()
	if _, err := io.WriteString(f.local, b); err != nil {
		t.Fatalf("write %q: %v", b, err)
	}
	_ = f.remote.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 1)
	if _, err := io.ReadFull(f.remote, got); err != nil || string(got) != b {
		t.Fatalf("forward lost %q: got %q err=%v", b, got, err)
	}
}

func (f *resetForwardFixture) cut(t *testing.T) bool {
	t.Helper()
	_ = f.remote.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := f.remote.Read(make([]byte, 1))
	return err != nil && !isTimeout(err)
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func (f *resetForwardFixture) failReconcile(t *testing.T) {
	t.Helper()
	f.clients[0].mu.Lock()
	f.clients[0].failReplace = true
	f.clients[0].mu.Unlock()
	if err := f.c.replaceGuardedRoutes(t.Context(), f.peers); err == nil {
		t.Fatal("reconcile with a failing guard succeeded")
	}
}

// A transient guard/name error resets the generation but must not cut an established forward; the
// machine's withdrawal on a later reconcile still cuts it at once.
func TestCoordinatorResetKeepsEstablishedForwardsUntilTheMachineIsWithdrawn(t *testing.T) {
	f := newResetForwardFixture(t)
	defer f.c.Stop()
	f.failReconcile(t)
	f.echo(t, "b")
	if err := f.c.replaceGuardedRoutes(t.Context(), f.peers); err != nil {
		t.Fatal(err)
	}
	f.echo(t, "c")
	if err := f.c.replaceGuardedRoutes(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if !f.cut(t) {
		t.Fatal("withdrawing the machine left the forward from before the reset open")
	}
}

func TestCoordinatorStopCutsForwardsKeptAcrossAReset(t *testing.T) {
	f := newResetForwardFixture(t)
	f.failReconcile(t)
	f.echo(t, "b")
	if err := f.c.Stop(); err != nil {
		t.Fatal(err)
	}
	if !f.cut(t) {
		t.Fatal("Stop left a forward kept across a reset open")
	}
}
