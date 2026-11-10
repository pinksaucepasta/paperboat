//go:build windows

package localapi

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// F14-02 on Windows: the daemon's open connections include the SSH sessions the hostd worker bridges, and a worker that
// cannot answer adds nothing (an update must never be blocked by a broken probe).
func TestWindowsOpenConnectionsIncludeWorkerBridgedSSH(t *testing.T) {
	pipe := testPipePath(t)
	var worker atomic.Int64
	var workerErr atomic.Bool
	server, err := NewServer(ServerConfig{
		SocketPath: pipe,
		OwnerSID:   testCurrentSID(t),
		Source:     windowsSnapshotSource(func(context.Context) (Snapshot, error) { return windowsValidSnapshot(1), nil }),
		WorkerOpenSSH: func(context.Context) (int, error) {
			if workerErr.Load() {
				return 0, errors.New("hostd unreachable")
			}
			return int(worker.Load()), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	client, err := waitForWindowsClient(pipe, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := client.OpenConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	worker.Store(2)
	if got, err := client.OpenConnections(ctx); err != nil || got.SSH != baseline.SSH+2 || got.Forwards != baseline.Forwards {
		t.Fatalf("with two worker sessions: %+v (err %v), baseline %+v", got, err, baseline)
	}
	workerErr.Store(true)
	if got, err := client.OpenConnections(ctx); err != nil || got.SSH != baseline.SSH {
		t.Fatalf("an unreachable worker changed the count: %+v (err %v)", got, err)
	}
}
