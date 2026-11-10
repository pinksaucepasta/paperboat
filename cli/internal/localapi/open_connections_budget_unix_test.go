//go:build darwin || linux

package localapi

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A slow but alive worker is still counted: the guard's budget (OpenConnectionsTimeout) outlasts the worker query, and a
// worker that never answers adds zero once its own budget ends.
func TestOpenConnectionsWaitsForASlowWorker(t *testing.T) {
	socket := filepath.Join(localAPITestDir(t), "budget.sock")
	var delay atomic.Int64
	delay.Store(int64(3 * time.Second))
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
		Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }),
		WorkerOpenSSH: func(ctx context.Context) (int, error) {
			select {
			case <-time.After(time.Duration(delay.Load())):
				return 2, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	waitForSocket(t, socket)
	guard, err := NewClient(socket, OpenConnectionsTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := guard.OpenConnections(ctx); err != nil || got.SSH < 2 {
		t.Fatalf("a slow worker was counted as zero: %+v (err %v)", got, err)
	}
	if OpenConnectionsTimeout <= WorkerOpenSSHTimeout {
		t.Fatal("the guard must outlast the worker query")
	}
}
