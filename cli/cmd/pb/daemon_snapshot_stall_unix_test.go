//go:build unix

package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

// F2-05: a daemon that accepts the connection and never answers (paused, or stalled while a revoked machine turns
// signed out) must not hold exec's snapshot peeks forever: exec printed its message and never exited.
func TestDaemonSnapshotPeeksAreBoundedByAStalledDaemon(t *testing.T) {
	socket := filepath.Join(commandRuntimeTestRoot(t), "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan net.Conn, 64) // accepted, held open, never answered
	t.Cleanup(func() {
		_ = listener.Close()
		for {
			select {
			case connection := <-held:
				_ = connection.Close()
			default:
				return
			}
		}
	})
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case held <- connection:
			default:
				_ = connection.Close()
			}
		}
	}()
	ctx := withHostDependencies(context.Background(), func(d *hostDependencies) {
		d.daemonPaths = func() (localapi.Paths, error) { return localapi.Paths{SocketPath: socket}, nil }
	})
	for name, read := range map[string]func(context.Context) (localapi.Snapshot, error){"warm": readWarmSnapshot, "lookup": readSnapshotWithLookupTimeout} {
		started := time.Now()
		if _, err := read(ctx); err == nil {
			t.Fatalf("%s: a stalled daemon answered", name)
		}
		if elapsed := time.Since(started); elapsed > daemonSnapshotLookupTimeout+2*time.Second {
			t.Fatalf("%s snapshot waited %s on a stalled daemon", name, elapsed)
		}
	}
	started := time.Now()
	daemonReportsControlPlaneUnavailable(ctx)
	if elapsed := time.Since(started); elapsed > daemonSnapshotLookupTimeout+2*time.Second {
		t.Fatalf("exec's control-plane peek waited %s on a stalled daemon", elapsed)
	}
}
