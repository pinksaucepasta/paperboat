package daemoncmd

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// leaseGuard is the machine guard's lease hold: one socket per port that outlives a daemon's
// connection, handed to whichever daemon generation acquires the port next.
type leaseGuard struct {
	mu      sync.Mutex
	sockets map[int]net.Listener
	accepts map[int]chan net.Conn
}

func (g *leaseGuard) client() guardedNameClient {
	return &leaseGuardClient{coordinatorNameClient: &coordinatorNameClient{}, guard: g}
}

func (g *leaseGuard) address(port int) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sockets[port].Addr().String()
}

func (g *leaseGuard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, socket := range g.sockets {
		_ = socket.Close()
	}
}

type leaseGuardClient struct {
	*coordinatorNameClient
	guard *leaseGuard
}

func (c *leaseGuardClient) AcquireMachine(ctx context.Context, _, _, hostname string, ip netip.Addr, port int) (net.Listener, netip.Addr, error) {
	listener, err := c.Acquire(ctx, hostname, ip, port)
	return listener, ip, err
}

func (c *leaseGuardClient) Acquire(_ context.Context, _ string, _ netip.Addr, port int) (net.Listener, error) {
	g := c.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sockets == nil {
		g.sockets, g.accepts = map[int]net.Listener{}, map[int]chan net.Conn{}
	}
	if g.sockets[port] == nil {
		socket, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		accepts := make(chan net.Conn)
		g.sockets[port], g.accepts[port] = socket, accepts
		go func() {
			for {
				conn, err := socket.Accept()
				if err != nil {
					return
				}
				accepts <- conn
			}
		}()
	}
	return &leasedListener{socket: g.sockets[port], accepts: g.accepts[port], done: make(chan struct{})}, nil
}

// leasedListener is one daemon's view of a held socket; closing it leaves the socket to the guard.
type leasedListener struct {
	socket  net.Listener
	accepts chan net.Conn
	done    chan struct{}
	once    sync.Once
}

func (l *leasedListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.accepts:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *leasedListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *leasedListener) Addr() net.Addr { return l.socket.Addr() }

func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(conn, conn); _ = conn.Close() }()
		}
	}()
	return listener.Addr().String()
}

func generationCoordinator(t *testing.T, guard *leaseGuard, echo string, dials *atomic.Int32) *Coordinator {
	t.Helper()
	c, err := NewCoordinator(CoordinatorConfig{NameClient: guard.client(), DialMachine: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", echo)
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	return c
}

func echoOnce(conn net.Conn, message string) error {
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(message)); err != nil {
		return err
	}
	got := make([]byte, len(message))
	_, err := io.ReadFull(conn, got)
	return err
}

// A connection forwarded through a user's local access keeps flowing across that user's daemon
// upgrade: the old generation drains (stops accepting, keeps its forwards), the new one takes the
// guard's held socket, new connects reach the new generation, and the old one is idle once its last
// stream closes. No gap above 1 s and no reconnect.
func TestHeldForwardSurvivesTheDaemonGenerationHandover(t *testing.T) {
	guard := &leaseGuard{}
	defer guard.close()
	echo := echoServer(t)
	var oldDials, newDials atomic.Int32
	peers := []*daemonrpc.PeerUpdate{{PeerId: "machine", Alias: "studio", AssignedIp: "127.100.0.31", Approved: true, Online: true, ExportedPorts: []int32{45678}}}
	old := generationCoordinator(t, guard, echo, &oldDials)
	defer old.Stop()
	if err := old.replaceGuardedRoutes(t.Context(), peers); err != nil {
		t.Fatal(err)
	}
	held, err := net.Dial("tcp", guard.address(45678))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := echoOnce(held, "before"); err != nil || oldDials.Load() != 1 {
		t.Fatalf("held stream through the old generation: %v (dials %d)", err, oldDials.Load())
	}
	before := liveconnections.Current().Forwards

	// The held stream echoes every 50 ms through the whole handover.
	stop, flowed := make(chan struct{}), make(chan error, 1)
	go func() {
		last := time.Now()
		for {
			select {
			case <-stop:
				flowed <- nil
				return
			case <-time.After(50 * time.Millisecond):
			}
			if err := echoOnce(held, "during"); err != nil {
				flowed <- err
				return
			}
			if gap := time.Since(last); gap > time.Second {
				flowed <- &gapError{gap}
				return
			}
			last = time.Now()
		}
	}()

	if err := old.Drain(); err != nil {
		t.Fatal(err)
	}
	next := generationCoordinator(t, guard, echo, &newDials)
	defer next.Stop()
	if err := next.replaceGuardedRoutes(t.Context(), peers); err != nil {
		t.Fatal(err)
	}
	fresh, err := net.Dial("tcp", guard.address(45678))
	if err != nil {
		t.Fatal(err)
	}
	if err := echoOnce(fresh, "new"); err != nil || newDials.Load() != 1 || oldDials.Load() != 1 {
		t.Fatalf("new connect after the handover: %v (new dials %d, old dials %d)", err, newDials.Load(), oldDials.Load())
	}
	_ = fresh.Close()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	if err := <-flowed; err != nil {
		t.Fatalf("held stream broke during the handover: %v", err)
	}
	if streams := old.DrainingStreams(); len(streams) != 1 {
		t.Fatalf("draining streams = %q, want the held one", streams)
	}
	_ = held.Close()
	deadline := time.Now().Add(2 * time.Second)
	for liveconnections.Current().Forwards >= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if liveconnections.Current().Forwards >= before {
		t.Fatal("the old generation still counts the closed held stream")
	}
}

type gapError struct{ gap time.Duration }

func (e *gapError) Error() string { return "gap " + e.gap.String() + " above 1 s" }
