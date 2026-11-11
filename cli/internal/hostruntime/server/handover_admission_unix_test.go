//go:build linux || darwin

package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// rotation hands each connection of the one shared runtime listener to one of
// the generations accepting on it, as the kernel does for two processes.
type rotation struct {
	mu    sync.Mutex
	next  int
	queue []chan net.Conn
}

func (r *rotation) set(queues ...chan net.Conn) { r.mu.Lock(); r.queue = queues; r.mu.Unlock() }
func (r *rotation) pick() chan net.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	return r.queue[r.next%len(r.queue)]
}

type queueListener struct {
	queue chan net.Conn
	done  chan struct{}
	addr  net.Addr
	once  sync.Once
}

func (l *queueListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.queue:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *queueListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *queueListener) Addr() net.Addr { return l.addr }

type handoverGeneration struct {
	controller *hostdproto.Controller
	queue      chan net.Conn
	refused    *atomic.Int32
	leases     *preview.OwnerSessionLeaseManager
	registry   *preview.RuntimeOwnerSessionRegistry
}

// startHandoverGeneration runs one generation's hostd controller, its
// workload bridge, its worker's dispatch handler and its preview lease manager.
func startHandoverGeneration(t *testing.T, ctx context.Context, name string, shared net.Listener, pending func() bool, leaseDir string) *handoverGeneration {
	t.Helper()
	config := hostdproto.ControllerConfig{APIMin: 1, APIMax: 1}
	if pending != nil {
		config.MaintenancePending = true
		config.VerifyMaintenanceCommitted = func(context.Context) error {
			if pending() {
				return hostdproto.ErrMaintenanceBusy
			}
			return nil
		}
	}
	controller, err := hostdproto.NewController(config)
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := controller.Negotiate(hostdproto.Hello{WorkerID: name, Version: "1", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.MarkReady(hostdproto.Ready{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}); err != nil {
		t.Fatal(err)
	}
	active, err := controller.Activate(hostdproto.Activate{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease})
	if err != nil {
		t.Fatal(err)
	}
	root := shorttemp.Dir(t)
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })
	executions, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, MaximumActive: 1, MaximumOperations: 2, ReplayBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Shutdown(context.Background()) })
	token := make([]byte, 32)
	token[0] = 9
	bridgeServer := &workloadbridge.Server{Endpoint: filepath.Join(root, "ipc", "owner.sock"), Token: token, Sessions: sessions, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 1}, Fence: controller.Status, AcquireActive: controller.AcquireActive, MaximumConcurrent: 64, Ready: make(chan error, 1)}
	bridgeCtx, stopBridge := context.WithCancel(ctx)
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridgeServer.Run(bridgeCtx) }()
	if err := <-bridgeServer.Ready; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBridge(); <-bridgeDone })
	bridge, err := workloadbridge.NewClient(bridgeServer.Endpoint, token, active.WorkerID, active.Epoch, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bridge.Close() })
	g := &handoverGeneration{controller: controller, queue: make(chan net.Conn), refused: new(atomic.Int32)}
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: "handover-capability-0123456789-abcdef", MaximumConcurrent: 64, HandshakeTimeout: time.Second,
		Fence: func(ctx context.Context) (func(), <-chan struct{}, error) {
			release, lost, err := bridge.AcquireAdmission(ctx)
			if err != nil {
				g.refused.Add(1)
			}
			return release, lost, err
		},
		Authorize: func(context.Context, streamauth.Header) (string, error) { return "access-session", nil },
		ServeStream: func(_ context.Context, _ streamauth.Header, c net.Conn) error {
			_, err := c.Write([]byte{1})
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	listener := &queueListener{queue: g.queue, done: make(chan struct{}), addr: shared.Addr()}
	worker := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = worker.Serve(listener) }()
	t.Cleanup(func() { _ = worker.Close(); _ = handler.Close(context.Background()) })
	g.registry, err = preview.NewRuntimeOwnerSessionRegistry(preview.RuntimeOwnerSessionRegistryConfig{MachineID: "machine_01", RuntimeDone: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	g.leases, err = preview.NewOwnerSessionLeaseManager(preview.OwnerSessionLeaseManagerConfig{MachineID: "machine_01", ControlToken: "control_secret", Registry: g.registry, HandoverDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// pbvm4 feat8: a pb update left 3-10 s in which no generation admitted new
// native work and foreground previews lost their owner. Every 50 ms a stream
// opens through the shared listener while a second generation starts with the
// update-pending fence, takes the listener over and the old one stops
// accepting; a foreground lease heartbeats whichever generation answers.
func TestHostdHandoverAdmitsEveryDispatchAndKeepsForegroundLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	shared, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	leaseDir := filepath.Join(t.TempDir(), "foreground-leases")
	var turn rotation
	old := startHandoverGeneration(t, ctx, "runtime-old", shared, nil, leaseDir)
	turn.set(old.queue)
	go func() {
		for {
			c, err := shared.Accept()
			if err != nil {
				return
			}
			select {
			case turn.pick() <- c:
			case <-ctx.Done():
				c.Close()
				return
			}
		}
	}()
	lease, err := old.leases.Acquire(preview.OwnerSessionLeaseRequest{Target: preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}}, "foreground_01")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: shared.Addr().String(), Token: "handover-capability-0123456789-abcdef", DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var committed atomic.Bool
	var leaseHolder atomic.Pointer[handoverGeneration]
	leaseHolder.Store(old)
	type result struct {
		at  time.Time
		err error
	}
	results := make(chan result, 256)
	stop := make(chan struct{})
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			header, _ := streamauth.New("operation", "exec", "stream", "allowed", time.Now().Add(5*time.Second), 1<<20)
			conn, err := client.Open(ctx, header)
			if err == nil {
				_, err = io.ReadFull(conn, make([]byte, 1))
				conn.Close()
			}
			results <- result{time.Now(), err}
		}
	}()
	heartbeatErr := make(chan error, 1)
	go func() {
		defer loops.Done()
		for {
			select {
			case <-stop:
				heartbeatErr <- nil
				return
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := leaseHolder.Load().leases.Heartbeat(lease.ID, lease.Token); err != nil {
				heartbeatErr <- err
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	// New generation: pending native commit, its worker active and serving the
	// shared listener next to the old one (worker_unix.go before commit).
	next := startHandoverGeneration(t, ctx, "runtime-next", shared, func() bool { return !committed.Load() }, leaseDir)
	if !next.registry.LocalOwnerAcquired(lease.OwnerSessionID) {
		t.Fatal("new generation does not hold the foreground owner before the CLI reaches it")
	}
	turn.set(old.queue, next.queue)
	time.Sleep(300 * time.Millisecond)
	// Handover commit: the old generation stops accepting and drains; the CLI's
	// heartbeats now reach only the new one.
	turn.set(next.queue)
	leaseHolder.Store(next)
	_ = old.leases.Close()
	time.Sleep(500 * time.Millisecond)
	committed.Store(true)
	if err := next.controller.CompleteMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	loops.Wait()
	close(results)
	if err := <-heartbeatErr; err != nil {
		t.Fatalf("foreground lease lost across the handover: %v", err)
	}
	var count int
	var last time.Time
	var worst time.Duration
	for r := range results {
		if r.err != nil {
			t.Fatalf("dispatch failed during the handover: %v", r.err)
		}
		if !last.IsZero() && r.at.Sub(last) > worst {
			worst = r.at.Sub(last)
		}
		last = r.at
		count++
	}
	if refused := old.refused.Load() + next.refused.Load(); refused != 0 || count < 20 || worst > time.Second {
		t.Fatalf("refused=%d dispatches=%d worst gap=%s", refused, count, worst)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("fixture timed out")
	}
	t.Logf("dispatches=%d worst gap=%s", count, worst)
}
