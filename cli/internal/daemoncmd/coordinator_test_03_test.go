package daemoncmd

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCoordinatorForwardRetainsConsumedFailureAndRecovers(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	reference := supportref.New()
	ctx := supportref.WithContext(t.Context(), reference)
	c := &Coordinator{cfg: CoordinatorConfig{DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, syscall.EIO }}}
	client, local := net.Pipe()
	c.forward(ctx, local, "PRIVATE machine", 1234)
	client.Close()
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference {
		t.Fatalf("consumed stream failure lost: %+v", faults)
	}
	c.cfg.DialMachine = func(context.Context, string, int) (net.Conn, error) { return nil, net.ErrClosed }
	client, local = net.Pipe()
	c.forward(ctx, local, "PRIVATE", 1234)
	client.Close()
	if len(faults) != 2 {
		t.Fatal("closed initial dial was suppressed as teardown")
	}
	c.cfg.DialMachine = func(context.Context, string, int) (net.Conn, error) { a, b := net.Pipe(); go b.Close(); return a, nil }
	client, local = net.Pipe()
	client.Close()
	c.forward(ctx, local, "PRIVATE", 1234)
	if len(faults) != 2 {
		t.Fatal("healthy teardown was observed as operational")
	}
}

func TestCoordinatorGenerationCleanupRetainsLateCause(t *testing.T) {
	release := make(chan struct{})
	client := &generationNameClient{generation: 1, stallRelease: release, releaseFailure: syscall.EIO}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: client, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.failReplace = true
	client.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = c.replaceGuardedRoutes(ctx, []*pbSync.PeerUpdate{peer})
	if !errors.Is(err, net.ErrClosed) || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("reconciliation lost deadline: %v", err)
	}
	close(release)
	if err := c.Stop(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("late generation cleanup lost: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("generation cleanup repeated: %v", err)
	}
}

func TestCoordinatorStartupRollbackRetainsOriginalCleanup(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: &coordinatorCleanupClient{cause: syscall.EIO}, SocketAddress: filepath.Join(blocked, "daemon.sock"), DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Start(t.Context())
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("startup cleanup lost: %v", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("startup original filesystem cause lost: %v", err)
	}
	if strings.Contains(err.Error(), blocked) {
		t.Fatal("startup private path exposed")
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("rollback double closed: %v", err)
	}
}

func TestCoordinatorReconciliationReportsOneEpisodeAndRecovery(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	reference := supportref.New()
	ctx := supportref.WithContext(t.Context(), reference)
	fail := true
	presentations := 0
	c, err := NewCoordinator(CoordinatorConfig{ConnectNameClient: func(context.Context) (guardedNameClient, error) {
		client := &coordinatorCleanupClient{}
		if fail {
			client.replaceCause = syscall.EIO
		}
		return client, nil
	}, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, nil }, ReportReconcileError: func(err error) {
		presentations++
		if err.Error() != "local machine access reconciliation failed" {
			t.Fatal("callback received raw failure")
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = ctx
	machines := []api.UserMachine{{ID: "PRIVATE", Alias: "studio", AssignedIP: "127.100.0.22", State: "online", Online: true, NetworkApproved: true}}
	for range 2 {
		if err := c.ApplyMachines(ctx, machines, nil); !errors.Is(err, syscall.EIO) {
			t.Fatalf("original reconciliation cause lost: %v", err)
		}
	}
	if len(faults) != 1 || presentations != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference || faults[0].Code != "local_access_machine_names_failed" || faults[0].Stage != "reconciliation" {
		t.Fatalf("episode ownership incorrect: %+v presentations %d", faults, presentations)
	}
	fail = false
	if err := c.ApplyMachines(ctx, machines, nil); err != nil {
		t.Fatal(err)
	}
	c.nameClient.(*coordinatorCleanupClient).replaceCause = syscall.EIO
	if err := c.ApplyMachines(ctx, machines, nil); !errors.Is(err, syscall.EIO) {
		t.Fatal("fresh failure lost")
	}
	if len(faults) != 2 || presentations != 2 {
		t.Fatal("healthy reconciliation did not reset incident")
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorFailedListenerWithdrawsReadinessAndReacquires(t *testing.T) {
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	machines := []api.UserMachine{{ID: "machine", Alias: "studio", AssignedIP: "127.100.0.22", State: "online", Online: true, NetworkApproved: true, InstallationGeneration: 2}}
	services := []api.MachineServicesMachine{{MachineID: "machine", InstallationGeneration: 2, ExpiresAt: time.Now().Add(time.Minute), Services: []api.MachineServicePort{{Port: 45678, AddressFamily: "ipv4"}}}}
	if err := c.ApplyMachines(t.Context(), machines, services); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	var old *guardedRoute
	for _, route := range c.routes {
		old = route
	}
	c.mu.Unlock()
	if old == nil {
		t.Fatal("native route not installed")
	}
	if err := old.listener.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		c.snapshotMu.Lock()
		failure := c.reconcileErr
		status, _ := c.rpcBackend.GetStatus(t.Context())
		c.snapshotMu.Unlock()
		if failure != nil && len(status.Peers) == 1 && len(status.Peers[0].ForwardedPorts) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed native listener remained ready")
		}
		time.Sleep(time.Millisecond)
	}
	if err := c.ApplyMachines(t.Context(), machines, services); err != nil {
		t.Fatal(err)
	}
	status, err := c.rpcBackend.GetStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Peers) != 1 || !slices.Contains(status.Peers[0].ForwardedPorts, int32(45678)) {
		t.Fatal("fresh authorized snapshot did not restore native readiness")
	}
	c.mu.Lock()
	for _, route := range c.routes {
		if route == old {
			t.Fatal("dead listener was reused")
		}
	}
	c.mu.Unlock()
	if err := c.ReconcileStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorRPCListenerFailureReachesOwnerAndRecovers(t *testing.T) {
	dir, err := os.MkdirTemp("", "pb-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c, err := NewCoordinator(CoordinatorConfig{SocketAddress: filepath.Join(dir, "daemon.sock")})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.rpcListener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-c.Errors():
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("RPC original listener failure lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RPC listener failure had no terminal owner")
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorStartRejectsUnfinishedGenerationCleanup(t *testing.T) {
	release := make(chan struct{})
	client := &generationNameClient{generation: 1, stallRelease: release}
	dir, err := os.MkdirTemp("", "pb-start-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c, err := NewCoordinator(CoordinatorConfig{NameClient: client, SocketAddress: filepath.Join(dir, "daemon.sock"), DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.failReplace = true
	client.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := c.replaceGuardedRoutes(ctx, []*pbSync.PeerUpdate{peer}); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	started := time.Now()
	if err := c.Start(t.Context()); !errors.Is(err, errCoordinatorCleanupRunning) || !strings.Contains(err.Error(), "wait for cleanup") || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("pending cleanup did not reject startup promptly: %v", err)
	}
	close(release)
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
}

// revokedListener fails its pending accept with EINVAL once released, as a socket the machine guard
// shut down does.
type revokedListener struct {
	net.Listener
	released chan struct{}
}

func (l *revokedListener) Accept() (net.Conn, error) {
	<-l.released
	return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EINVAL)}
}
func (l *revokedListener) Close() error { close(l.released); return nil }

// Releasing a route while it is still registered makes the machine guard shut its socket down, which
// fails the daemon's pending accept with EINVAL. That is the daemon closing the route itself: it must
// not retire the route as failed or degrade local access.
func TestCoordinatorClosingARouteIsNotAListenerFailure(t *testing.T) {
	reported := make(chan error, 1)
	c, err := NewCoordinator(CoordinatorConfig{NameClient: &generationNameClient{generation: 1}, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, net.ErrClosed }, ReportReconcileError: func(err error) { reported <- err }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	route := &guardedRoute{listener: &revokedListener{released: make(chan struct{})}, machineID: "peer", port: 8080, ctx: ctx, cancel: cancel}
	c.routes["peer\x00studio\x00127.100.0.22\x008080"] = route
	c.wg.Add(1)
	go c.serveRoute(route)
	if err := route.close(); err != nil {
		t.Fatal(err)
	}
	c.wg.Wait()
	select {
	case err := <-reported:
		t.Fatalf("closing a route degraded local access: %v", err)
	default:
	}
	if len(c.routes) != 1 {
		t.Fatalf("route was retired as failed: %d routes", len(c.routes))
	}
}
