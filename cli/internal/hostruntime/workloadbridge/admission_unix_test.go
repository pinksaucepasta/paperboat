//go:build linux || darwin

package workloadbridge

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func TestAcceptedFeatureAdmissionHoldsNativeMaintenanceUntilReleased(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Shutdown(context.Background())
	executions, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, MaximumActive: 1, MaximumOperations: 2, ReplayBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer executions.Shutdown(context.Background())
	controller, err := hostdproto.NewController(hostdproto.ControllerConfig{APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := controller.Negotiate(hostdproto.Hello{WorkerID: "worker", Version: "1", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	ready := hostdproto.Ready{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}
	if err := controller.MarkReady(ready); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Activate(hostdproto.Activate{WorkerID: ready.WorkerID, APIVersion: ready.APIVersion, Epoch: ready.Epoch, Lease: ready.Lease}); err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 32)
	token[0] = 7
	server := &Server{Endpoint: filepath.Join(root, "ipc", "owner.sock"), Token: token, Sessions: sessions, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 1}, Fence: controller.Status, AcquireActive: controller.AcquireActive, MaximumConcurrent: 4, Ready: make(chan error, 1)}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client, err := NewClient(server.Endpoint, token, "worker", welcome.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	release, lost, err := client.AcquireAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	bounded, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	if _, err := controller.PrepareMaintenance(bounded, false, nil); !errors.Is(err, hostdproto.ErrMaintenanceBusy) {
		stop()
		t.Fatalf("accepted stream did not hold maintenance: %v", err)
	}
	stop()
	release()
	select {
	case <-lost:
	case <-ctx.Done():
		t.Fatal("released lease remained connected")
	}
	if _, err := controller.PrepareMaintenance(ctx, false, nil); err != nil {
		t.Fatal(err)
	}
	controller.AbortMaintenance()
	stale, err := NewClient(server.Endpoint, token, "worker", welcome.Epoch+1)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	if release, _, err := stale.AcquireAdmission(ctx); !errors.Is(err, ErrFenced) {
		if release != nil {
			release()
		}
		t.Fatalf("stale admission=%v", err)
	}
	release, lost, err = client.AcquireAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-lost:
	case <-time.After(time.Second):
		t.Fatal("native shutdown left accepted lease live")
	}
	release()
}
