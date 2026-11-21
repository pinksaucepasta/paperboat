package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
	"github.com/pinksaucepasta/paperboat/cli/internal/testcert"
)

// A guard connection that stops answering because the guard restarted.
type restartableGuardClient struct {
	*coordinatorNameClient
	dead      atomic.Bool
	statusErr error // returned by Status once, as a guard that is restarting
	mu        sync.Mutex
	listeners []net.Listener
}

func (c *restartableGuardClient) AcquireMachine(ctx context.Context, account, machine, hostname string, ip netip.Addr, port int) (net.Listener, netip.Addr, error) {
	listener, assigned, err := c.coordinatorNameClient.AcquireMachine(ctx, account, machine, hostname, ip, port)
	if err == nil {
		c.mu.Lock()
		c.listeners = append(c.listeners, listener)
		c.mu.Unlock()
	}
	return listener, assigned, err
}

// restart is what a replaced root guard does to this connection: the answers stop and every listener it held closes.
func (c *restartableGuardClient) restart() {
	c.dead.Store(true)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, listener := range c.listeners {
		_ = listener.Close()
	}
}

func (c *restartableGuardClient) Status(ctx context.Context) (machineguard.RuntimeStatus, error) {
	if c.dead.Load() {
		return machineguard.RuntimeStatus{}, errors.New("machine guard connection closed")
	}
	if err := c.statusErr; err != nil {
		c.statusErr = nil
		return machineguard.RuntimeStatus{}, err
	}
	return c.coordinatorNameClient.Status(ctx)
}

// After the root guard restarts, a daemon whose reconcile had succeeded reconnects and re-acquires its routes
// by itself within a bound: no restart of the daemon and no new topology snapshot.
func TestCoordinatorReconnectsToARestartedGuardWithoutANewSnapshot(t *testing.T) {
	reconnectsToARestartedGuard(t, 10*time.Millisecond, func(c *restartableGuardClient) { c.dead.Store(true) })
}

// The listeners a restarting guard closes tell the daemon at once: with the periodic watch effectively off, the
// failed listener alone makes it reconnect, so another OS user's install does not leave this one without access.
func TestCoordinatorReconnectsAtOnceWhenARestartedGuardClosesItsListeners(t *testing.T) {
	reconnectsToARestartedGuard(t, time.Hour, (*restartableGuardClient).restart)
}

func reconnectsToARestartedGuard(t *testing.T, watch time.Duration, kill func(*restartableGuardClient)) {
	t.Helper()
	restoreWatch := guardWatchInterval
	guardWatchInterval = watch
	t.Cleanup(func() { guardWatchInterval = restoreWatch })
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	var clients []*restartableGuardClient
	c, err := NewCoordinator(CoordinatorConfig{
		RetryBase: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond,
		SocketAddress: socketAddress(shorttemp.Dir(t)),
		ConnectNameClient: func(context.Context) (guardedNameClient, error) {
			client := &restartableGuardClient{coordinatorNameClient: &coordinatorNameClient{}}
			clients = append(clients, client)
			return client, nil
		},
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") },
		IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
			cert, key, err := ca.IssueCertificate([]string{name})
			if err != nil {
				return tls.Certificate{}, err
			}
			return tls.X509KeyPair(cert, key)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "laptop", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err = c.reconcileSnapshot(t.Context(), []*daemonrpc.PeerUpdate{peer}, 1); err != nil {
		t.Fatal(err)
	}
	if step := c.PendingLocalAccessStep(); step != "" || len(clients) != 1 {
		t.Fatalf("setup: pending step %q, %d guard connections; want a clean first reconcile on one", step, len(clients))
	}
	first := clients[len(clients)-1]
	kill(first)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		routes, current := len(c.routes), c.nameClient
		c.mu.Unlock()
		if routes == 1 && current != nil && current != guardedNameClient(first) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not reconnect to the restarted guard: routes=%d connects=%d", routes, len(clients))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var errRefusal = errors.New("guard refused")

// A guard that times out once (it is restarting) is dialed again inside the same reconcile, which then succeeds
// without a pending step; a refusal is not retried.
func TestCoordinatorRetriesATransientGuardFailureInTheSameReconcile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		first      error
		wantDials  int
		wantFailed bool
	}{
		{"timeout", os.ErrDeadlineExceeded, 2, false},
		{"closed", net.ErrClosed, 2, false},
		{"refusal", errRefusal, 1, true},
		{"cancelled", context.Canceled, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dials := 0
			c, err := NewCoordinator(CoordinatorConfig{
				SocketAddress: socketAddress(shorttemp.Dir(t)),
				ConnectNameClient: func(context.Context) (guardedNameClient, error) {
					dials++
					client := &restartableGuardClient{coordinatorNameClient: &coordinatorNameClient{}}
					if dials == 1 {
						if tc.first == context.Canceled {
							cancel() // the reconcile itself ends: never retried
						}
						client.statusErr = tc.first
					}
					return client, nil
				},
				DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = c.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Stop() })
			peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "laptop", AssignedIp: "127.100.0.23", Approved: true, Online: true, ExportedPorts: []int32{8080}}
			err = c.reconcileSnapshot(ctx, []*daemonrpc.PeerUpdate{peer}, 1)
			if dials != tc.wantDials || (err != nil) != tc.wantFailed || c.PendingLocalAccessStep() != "" && !tc.wantFailed {
				t.Fatalf("dials=%d err=%v; want dials=%d failed=%v", dials, err, tc.wantDials, tc.wantFailed)
			}
			var op coordinatorOperationError
			if tc.wantFailed && (!errors.As(err, &op) || op.LocalAccessStep() != "guard_status" || !errors.Is(err, tc.first)) {
				t.Fatalf("err = %v; want a typed guard_status failure carrying %v", err, tc.first)
			}
		})
	}
}
