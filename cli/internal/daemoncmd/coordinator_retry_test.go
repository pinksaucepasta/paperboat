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
	"github.com/pinksaucepasta/paperboat/internal/testcert"
)

// A browser certificate that fails once at start (a second macOS user's daemon on pbmac timed out issuing it)
// comes back by the coordinator's own retry: no restart and no new topology snapshot.
func TestCoordinatorRetriesTheBrowserGatewayAfterAFailedCertificate(t *testing.T) {
	restoreBase, restoreMax := reconcileRetryBase, reconcileRetryMax
	reconcileRetryBase, reconcileRetryMax = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { reconcileRetryBase, reconcileRetryMax = restoreBase, restoreMax })
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	var issued atomic.Int32
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{
		NameClient:  names,
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") },
		IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
			if issued.Add(1) == 1 {
				return tls.Certificate{}, errors.New("certificate issuance timed out")
			}
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
	c.mu.Lock()
	c.ctx, c.running = t.Context(), true
	c.mu.Unlock()
	t.Cleanup(c.resetReconcileRetry)
	peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "hp", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err := c.reconcileSnapshot(t.Context(), []*daemonrpc.PeerUpdate{peer}, 1); err != nil {
		t.Fatalf("a browser certificate failure must keep native access: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		pending, web := c.browserPending, len(c.webRoutes)
		c.mu.Unlock()
		if !pending && web == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("browser gateway did not recover by retry: pending=%v webRoutes=%d issued=%d", pending, web, issued.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if issued.Load() < 2 {
		t.Fatalf("certificate issued %d times; want a retry", issued.Load())
	}
}

// A machine name the guard refuses degrades only that item (multi-account hosts): the reconcile succeeds, native
// access stays, doctor's pending step is machine_names, and the retry publishes the names once the guard accepts.
func TestCoordinatorKeepsLocalAccessWhenTheGuardRefusesANameAndRetries(t *testing.T) {
	restoreBase, restoreMax := reconcileRetryBase, reconcileRetryMax
	reconcileRetryBase, reconcileRetryMax = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { reconcileRetryBase, reconcileRetryMax = restoreBase, restoreMax })
	names := &coordinatorNameClient{refuseNames: 1}
	c, err := NewCoordinator(CoordinatorConfig{
		NameClient:  names,
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") },
		IssueCertificate: func(context.Context, string, string, int) (tls.Certificate, error) {
			return tls.Certificate{}, errors.New("no browser trust in this test")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.ctx, c.running = t.Context(), true
	c.mu.Unlock()
	t.Cleanup(c.resetReconcileRetry)
	peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "hp", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err := c.reconcileSnapshot(t.Context(), []*daemonrpc.PeerUpdate{peer}, 1); err != nil {
		t.Fatalf("a refused name must not fail local access: %v", err)
	}
	c.mu.Lock()
	routes := len(c.routes)
	c.mu.Unlock()
	if routes != 1 {
		t.Fatalf("native access was withdrawn for a refused name: routes=%d", routes)
	}
	if step := c.PendingLocalAccessStep(); step != "machine_names" {
		t.Fatalf("pending step=%q, want machine_names", step)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.PendingLocalAccessStep() == "machine_names" {
		if time.Now().After(deadline) {
			t.Fatal("the refused name was not retried")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
