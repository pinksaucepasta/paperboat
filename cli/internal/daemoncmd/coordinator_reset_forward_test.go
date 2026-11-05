package daemoncmd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
)

// resetForwardFixture is a coordinator with one forwarded connection open through a guarded route.
type resetForwardFixture struct {
	c          *Coordinator
	clients    []*coordinatorNameClient
	peers      []*daemonrpc.PeerUpdate
	local      net.Conn
	remote     net.Conn
	connectErr error // set: the guard is unavailable for every later connect
}

// shutdownGuardClient is the guard's behaviour when its control connection closes: it withdraws the leases,
// shutting down every listening socket it handed out (the daemon's SCM_RIGHTS copy fails Accept at once),
// and only then does the close return.
type shutdownGuardClient struct {
	*coordinatorNameClient
	mu        sync.Mutex
	listeners []net.Listener
}

func (g *shutdownGuardClient) AcquireMachine(ctx context.Context, account, machineID, hostname string, ip netip.Addr, port int) (net.Listener, netip.Addr, error) {
	listener, assigned, err := g.coordinatorNameClient.AcquireMachine(ctx, account, machineID, hostname, ip, port)
	if err == nil {
		g.mu.Lock()
		g.listeners = append(g.listeners, listener)
		g.mu.Unlock()
	}
	return listener, assigned, err
}

func (g *shutdownGuardClient) Close() error {
	g.mu.Lock()
	for _, listener := range g.listeners {
		_ = listener.Close()
	}
	g.mu.Unlock()
	time.Sleep(100 * time.Millisecond) // the withdraw round trip: the daemon's accept loop wakes meanwhile
	return g.coordinatorNameClient.Close()
}

func newResetForwardFixture(t *testing.T) *resetForwardFixture {
	t.Helper()
	f := &resetForwardFixture{clients: []*coordinatorNameClient{{}, {}, {}}}
	opened := make(chan net.Conn, 1)
	next := 0
	c, err := NewCoordinator(CoordinatorConfig{
		ConnectNameClient: func(context.Context) (guardedNameClient, error) {
			if f.connectErr != nil {
				return nil, f.connectErr
			}
			client := f.clients[next]
			next++
			return &shutdownGuardClient{coordinatorNameClient: client}, nil
		},
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
	f.peers = []*daemonrpc.PeerUpdate{{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080, 9090}}}
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

// Review #3: a snapshot that withdraws the machine cuts a forward kept across a reset even while the guard is
// still unavailable; withdrawal must not depend on rebuilding the local listeners.
func TestCoordinatorWithdrawalCutsDrainingForwardsWhileTheGuardIsUnavailable(t *testing.T) {
	f := newResetForwardFixture(t)
	defer f.c.Stop()
	f.failReconcile(t)
	f.echo(t, "b")
	f.connectErr = errors.New("guard unavailable")
	if err := f.c.replaceGuardedRoutes(t.Context(), nil); err == nil {
		t.Fatal("reconcile without a guard succeeded")
	}
	if !f.cut(t) {
		t.Fatal("a withdrawn machine's forward survived because the guard was unavailable")
	}
}

// Review #4: withdrawing one exported port (8080) while the machine keeps another (9090) cuts the 8080
// forward kept across a reset, exactly as it would without the reset.
func TestCoordinatorWithdrawingOnePortCutsItsDrainingForward(t *testing.T) {
	f := newResetForwardFixture(t)
	defer f.c.Stop()
	f.failReconcile(t)
	f.echo(t, "b")
	only9090 := []*daemonrpc.PeerUpdate{{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{9090}}}
	if err := f.c.replaceGuardedRoutes(t.Context(), only9090); err != nil {
		t.Fatal(err)
	}
	if !f.cut(t) {
		t.Fatal("the withdrawn port's forward survived because the machine kept another port")
	}
}

// Review #2: the guard shuts the shared listening sockets down when its client closes, so the accept loop can
// fail before the route stops accepting; the route must already be draining, or the forward is cut.
func TestCoordinatorResetKeepsForwardsWhenTheGuardShutsTheListenerFirst(t *testing.T) {
	for i := 0; i < 5; i++ {
		f := newResetForwardFixture(t)
		f.failReconcile(t)
		f.echo(t, "b")
		_ = f.c.Stop()
	}
}
