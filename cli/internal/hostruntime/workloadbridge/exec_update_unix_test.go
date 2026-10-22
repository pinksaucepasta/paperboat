//go:build darwin || linux

package workloadbridge

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func TestBridgeCanceledExecUpdateDrainReopensAdmission(t *testing.T) {
	root := shorttemp.Dir(t)
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) {
		return adapter.Start(command)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Shutdown(context.Background())
	executions, err := execprocess.New(execprocess.Config{
		WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"},
		MaximumActive: 2, MaximumOperations: 8, ReplayBytes: 64 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := executions.Shutdown(shutdownCtx); err != nil {
			t.Errorf("exec manager shutdown: %v", err)
		}
	}()

	serverCtx, stopServer := context.WithCancel(context.Background())
	token := make([]byte, 32)
	token[0] = 0x64
	server := &Server{
		Endpoint: filepath.Join(shorttemp.Dir(t), "socket", "owner.sock"), Token: token,
		Sessions: sessions, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 2},
		MaximumConcurrent: 8, Ready: make(chan error, 1),
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1, APIVersion: 1}
		},
		AcquireActive: func(worker string, epoch uint64) (func(), error) {
			if worker != "worker" || epoch != 1 {
				return nil, hostdproto.ErrFenced
			}
			return func() {}, nil
		},
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(serverCtx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopServer()
		if err := <-serverDone; err != nil {
			t.Errorf("owner bridge shutdown: %v", err)
		}
	})
	client, err := NewClient(server.Endpoint, token, "worker", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, err := client.Executions()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	running, _, err := remote.Start(ctx, execprocess.Request{
		OperationID: "operation_bridge_update_running",
		Argv:        []string{"/bin/sh", "-c", "sleep 0.4"},
		CWD:         root,
	})
	if err != nil {
		t.Fatal(err)
	}
	drainCtx, cancelDrain := context.WithTimeout(ctx, 20*time.Millisecond)
	drainErr := remote.BeginUpdate(drainCtx, "bridge_update_cancel")
	cancelDrain()
	if drainErr == nil {
		t.Fatal("update drain completed while the command was still running")
	}
	if snapshot := running.Snapshot(); snapshot.State == execprocess.StateExited || snapshot.State == execprocess.StateCanceled || snapshot.State == execprocess.StateFailed {
		t.Fatalf("canceled bridge drain terminated the command: %+v", snapshot)
	}

	deadline := time.Now().Add(time.Second)
	for {
		_, _, err = remote.Start(ctx, execprocess.Request{
			OperationID: "operation_bridge_update_reopened",
			Argv:        []string{"/bin/true"},
			CWD:         root,
		})
		if err == nil {
			break
		}
		if !errors.Is(err, execprocess.ErrUpdateInProgress) || time.Now().After(deadline) {
			t.Fatalf("canceled drain did not reopen authenticated exec admission: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := running.Wait(ctx); err != nil {
		t.Fatalf("running command did not finish naturally after cancellation: %v", err)
	}
}
