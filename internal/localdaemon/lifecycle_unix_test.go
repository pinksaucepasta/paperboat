//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestFailedStartupReleasesWorkersAndProcessLock(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	paths := daemonTestPaths(t)
	// Fail after transport ownership starts, before inventory or IPC startup.
	if err := os.WriteFile(filepath.Join(paths.StateRoot, "diagnostics"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	source := &scriptedMachineSource{}
	if err := Run(context.Background(), DaemonConfig{Paths: paths, Source: source, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid()}); err == nil {
		t.Fatal("startup accepted a non-directory diagnostics path")
	}
	lock, err := acquireProcessLock(paths.LockPath, os.Geteuid())
	if err != nil {
		t.Fatalf("failed startup retained its process lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// This is the same startup boundary already enforced by the Windows daemon.
type blockingStartupSource struct{}

func (blockingStartupSource) ListUserMachines(ctx context.Context) ([]api.UserMachine, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestDaemonServesStartingSnapshotDuringStalledRefresh(t *testing.T) {
	paths := daemonTestPaths(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, DaemonConfig{Paths: paths, Source: blockingStartupSource{}, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), RequestTimeout: 30 * time.Second})
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("daemon cleanup timed out")
		}
	})
	client, err := localapi.NewClient(paths.SocketPath, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	var snapshot localapi.Snapshot
	for time.Now().Before(deadline) {
		snapshot, err = client.Snapshot(context.Background())
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || snapshot.DaemonState != "starting" || snapshot.Generation != 1 {
		t.Fatalf("startup snapshot state=%q error=%v", snapshot.DaemonState, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled inventory survived daemon cancellation")
	}
}
