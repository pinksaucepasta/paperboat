package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/testcert"
)

// A guard connection that stops answering because the guard restarted.
type restartableGuardClient struct {
	*coordinatorNameClient
	dead atomic.Bool
}

func (c *restartableGuardClient) Status(ctx context.Context) (machineguard.RuntimeStatus, error) {
	if c.dead.Load() {
		return machineguard.RuntimeStatus{}, errors.New("machine guard connection closed")
	}
	return c.coordinatorNameClient.Status(ctx)
}

// After the root guard restarts, a daemon whose reconcile had succeeded reconnects and re-acquires its routes
// by itself within a bound: no restart of the daemon and no new topology snapshot.
func TestCoordinatorReconnectsToARestartedGuardWithoutANewSnapshot(t *testing.T) {
	restoreBase, restoreMax, restoreWatch := reconcileRetryBase, reconcileRetryMax, guardWatchInterval
	reconcileRetryBase, reconcileRetryMax, guardWatchInterval = 10*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { reconcileRetryBase, reconcileRetryMax, guardWatchInterval = restoreBase, restoreMax, restoreWatch })
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	var clients []*restartableGuardClient
	c, err := NewCoordinator(CoordinatorConfig{
		SocketAddress: t.TempDir() + "/daemon.sock",
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
	peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "hp", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err = c.reconcileSnapshot(t.Context(), []*daemonrpc.PeerUpdate{peer}, 1); err != nil {
		t.Fatal(err)
	}
	if step := c.PendingLocalAccessStep(); step != "" || len(clients) != 1 {
		t.Fatalf("setup: pending step %q, %d guard connections; want a clean first reconcile on one", step, len(clients))
	}
	first := clients[len(clients)-1]
	first.dead.Store(true)
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
