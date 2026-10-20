//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

func TestSuppliedMaintenanceBoundary(t *testing.T) {
	root, rootErr := os.MkdirTemp("/tmp", "pbsup-")
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	token := make([]byte, 32)
	socket := filepath.Join(root, "h.sock")
	tokenPath := filepath.Join(root, "token")
	if err := os.WriteFile(tokenPath, token, 0600); err != nil {
		t.Fatal(err)
	}
	server, err := hostdproto.NewServer(hostdproto.SocketConfig{SocketPath: socket, StatePath: filepath.Join(root, "state", "fence.json"), UID: os.Getuid(), GID: os.Getgid(), Token: token, APIMin: 1, APIMax: 1, Workloads: func() hostdproto.WorkloadStatus { return hostdproto.WorkloadStatus{Generation: 1, Protected: 1} }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client, err := hostdproto.NewClient(socket, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, err = client.Active(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = PrepareSuppliedMaintenance(context.Background(), socket, tokenPath, false); !errors.Is(err, hostdproto.ErrMaintenanceBusy) {
		t.Fatalf("ordinary replacement protected workload error=%v", err)
	}
	abort, err := PrepareSuppliedMaintenance(context.Background(), socket, tokenPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareSuppliedMaintenance(context.Background(), socket, tokenPath, true); !errors.Is(err, hostdproto.ErrMaintenanceBusy) {
		t.Fatalf("concurrent maintenance accepted: %v", err)
	}
	abort()
	abort, err = PrepareSuppliedMaintenance(context.Background(), socket, tokenPath, true)
	if err != nil {
		t.Fatal("abort did not reopen admission", err)
	}
	abort()
	wrongToken := append([]byte(nil), token...)
	wrongToken[0] = 1
	if err = os.WriteFile(tokenPath, wrongToken, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareSuppliedMaintenance(context.Background(), socket, tokenPath, true); err == nil {
		t.Fatal("incorrect installation capability accepted")
	}
}

// Device finding: pb update --from with a 45s exec running failed with the
// "approve administrator access" text instead of the busy outcome, because the
// bounded wait for running work ended as a bare client timeout.
func TestSuppliedMaintenanceWaitTimeoutIsTheBusyOutcome(t *testing.T) {
	root, rootErr := os.MkdirTemp("/tmp", "pbbusy-")
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	token := make([]byte, 32)
	socket := filepath.Join(root, "h.sock")
	tokenPath := filepath.Join(root, "token")
	if err := os.WriteFile(tokenPath, token, 0600); err != nil {
		t.Fatal(err)
	}
	server, err := hostdproto.NewServer(hostdproto.SocketConfig{SocketPath: socket, StatePath: filepath.Join(root, "state", "fence.json"), UID: os.Getuid(), GID: os.Getgid(), Token: token, APIMin: 1, APIMax: 1,
		Workloads:       func() hostdproto.WorkloadStatus { return hostdproto.WorkloadStatus{Generation: 1} },
		DrainExecutions: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		client, clientErr := hostdproto.NewClient(socket, token, time.Second)
		if clientErr == nil {
			if _, err = client.Active(context.Background()); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	_, err = prepareMaintenance(context.Background(), socket, tokenPath, false, 300*time.Millisecond)
	var busy *MaintenanceBusyTimeoutError
	if !errors.Is(err, hostdproto.ErrMaintenanceBusy) || !errors.As(err, &busy) || time.Since(start) > 3*time.Second {
		t.Fatalf("running work past the wait bound: err=%v after %s", err, time.Since(start))
	}
}
