//go:build darwin || linux

package hostdproto

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
)

func maintenanceExecManager(t *testing.T) (*execprocess.Manager, string) {
	t.Helper()
	root := t.TempDir()
	manager, err := execprocess.New(execprocess.Config{
		WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"},
		MaximumActive: 2, MaximumOperations: 8, ReplayBytes: 64 << 10, ChunkBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("exec manager cleanup: %v", err)
		}
	})
	return manager, filepath.Clean(root)
}

func TestMaintenanceFencesAdmissionsWhileExecFinishesNaturally(t *testing.T) {
	manager, root := maintenanceExecManager(t)
	command, _, err := manager.Start(context.Background(), execprocess.Request{
		OperationID: "operation_maintenance_drain",
		Argv:        []string{"/bin/sh", "-c", "sleep 0.3; printf finished"},
		CWD:         root,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{active: worker{workerID: "worker", apiVersion: 1, epoch: 1}, now: time.Now}
	drainEntered := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := c.PrepareMaintenanceWithDrain(context.Background(), false, func() WorkloadStatus {
			return WorkloadStatus{Generation: 1, Protected: uint64(len(manager.ActiveSnapshots()))}
		}, func(ctx context.Context) error {
			close(drainEntered)
			return manager.WaitIdle(ctx)
		})
		result <- err
	}()
	select {
	case <-drainEntered:
	case <-time.After(time.Second):
		t.Fatal("native maintenance did not begin the exec drain")
	}
	if _, err := c.AcquireActive("worker", 1); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("new hostd mutation crossed native maintenance fence: %v", err)
	}
	if snapshot := command.Snapshot(); snapshot.State == execprocess.StateExited || snapshot.State == execprocess.StateCanceled || snapshot.State == execprocess.StateFailed {
		t.Fatalf("native drain terminated command before it could finish: %+v", snapshot)
	}
	select {
	case err := <-result:
		t.Fatalf("maintenance returned before running exec completed: %v", err)
	default:
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("maintenance after natural exec exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("maintenance did not finish after the exec exited")
	}
	snapshot, err := command.Wait(context.Background())
	if err != nil || snapshot.State != execprocess.StateExited || snapshot.Result == nil || snapshot.Result.Code != 0 {
		t.Fatalf("exec did not finish naturally: snapshot=%+v err=%v", snapshot, err)
	}
	c.AbortMaintenance()
	if _, err := c.AcquireActive("worker", 1); err != nil {
		t.Fatalf("aborted maintenance did not reopen hostd admission: %v", err)
	}
}

func TestCanceledNativeDrainReopensFenceAndLeavesExecRunning(t *testing.T) {
	manager, root := maintenanceExecManager(t)
	command, _, err := manager.Start(context.Background(), execprocess.Request{
		OperationID: "operation_maintenance_cancel",
		Argv:        []string{"/bin/sh", "-c", "sleep 0.3"},
		CWD:         root,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{active: worker{workerID: "worker", apiVersion: 1, epoch: 1}, now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.PrepareMaintenanceWithDrain(ctx, false, nil, manager.WaitIdle)
	if !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("canceled native drain error=%v", err)
	}
	if _, err := c.AcquireActive("worker", 1); err != nil {
		t.Fatalf("canceled native drain left admission fenced: %v", err)
	}
	if snapshot := command.Snapshot(); snapshot.State == execprocess.StateExited || snapshot.State == execprocess.StateCanceled || snapshot.State == execprocess.StateFailed {
		t.Fatalf("canceled native drain terminated command: %+v", snapshot)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	snapshot, err := command.Wait(waitCtx)
	if err != nil || snapshot.State != execprocess.StateExited || snapshot.Result == nil || snapshot.Result.Code != 0 {
		t.Fatalf("command did not survive canceled drain: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestCanceledSocketMaintenanceReopensAfterPeerDisconnect(t *testing.T) {
	manager, root := maintenanceExecManager(t)
	command, _, err := manager.Start(context.Background(), execprocess.Request{
		OperationID: "operation_socket_maintenance_cancel",
		Argv:        []string{"/bin/sh", "-c", "sleep 0.3"},
		CWD:         root,
	})
	if err != nil {
		t.Fatal(err)
	}

	config := testSocketConfig(t)
	drainEntered := make(chan struct{})
	config.Workloads = func() WorkloadStatus { return WorkloadStatus{Generation: 1} }
	config.DrainExecutions = func(ctx context.Context) error {
		close(drainEntered)
		return manager.WaitIdle(ctx)
	}
	server, stop, serverDone := startSocketServer(t, config)
	t.Cleanup(func() { stopSocketServer(t, stop, serverDone) })
	client := testSocketClient(t, config)
	welcome := negotiateSocket(t, client, "runtime-maintenance")
	if _, err := client.Request(context.Background(), readyFor(welcome)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), activateFor(welcome)); err != nil {
		t.Fatal(err)
	}

	prepareCtx, cancelPrepare := context.WithCancel(context.Background())
	prepareDone := make(chan error, 1)
	go func() { prepareDone <- client.PrepareMaintenance(prepareCtx, false) }()
	select {
	case <-drainEntered:
	case <-time.After(time.Second):
		cancelPrepare()
		t.Fatal("socket maintenance did not enter the execution drain")
	}
	cancelPrepare()
	if err := <-prepareDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled socket maintenance error=%v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		release, err := server.AcquireActive("runtime-maintenance", welcome.Epoch)
		if err == nil {
			release()
			break
		}
		if !errors.Is(err, ErrMaintenanceBusy) || time.Now().After(deadline) {
			t.Fatalf("peer cancellation left native admission fenced: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snapshot := command.Snapshot(); snapshot.State == execprocess.StateExited || snapshot.State == execprocess.StateCanceled || snapshot.State == execprocess.StateFailed {
		t.Fatalf("peer cancellation terminated the running command: %+v", snapshot)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	snapshot, err := command.Wait(waitCtx)
	if err != nil || snapshot.State != execprocess.StateExited || snapshot.Result == nil || snapshot.Result.Code != 0 {
		t.Fatalf("command did not survive peer cancellation: snapshot=%+v err=%v", snapshot, err)
	}
}
