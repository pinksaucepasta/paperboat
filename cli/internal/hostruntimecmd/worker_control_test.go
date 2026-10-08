//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"os"
	"path/filepath"
	"testing"
)

type registryWorker struct {
	ctx                                context.Context
	readyCalls, activeCalls, stopCalls int
	stopErr                            error
}

func (w *registryWorker) Ready(context.Context) (hostdproto.Status, error) {
	w.readyCalls++
	return hostdproto.Status{State: hostdproto.StateCandidate, WorkerID: "runtime-test", Epoch: 1, APIVersion: 1}, nil
}
func (w *registryWorker) Activate(context.Context) (hostdproto.Status, error) {
	w.activeCalls++
	return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-test", Epoch: 1, APIVersion: 1}, nil
}
func (w *registryWorker) Stop(context.Context) error { w.stopCalls++; return w.stopErr }
func TestOwnedWorkersRetainsCommittedWorkerAcrossUpdaterCancellationAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	worker := &registryWorker{}
	starts := 0
	registry := newOwnedWorkers(ownerCtx, workerupdate.StartRequest{Executable: path, MutationsDisabled: true}, func(ctx context.Context, r workerupdate.StartRequest) (workerupdate.Worker, error) {
		starts++
		worker.ctx = ctx
		return worker, nil
	})
	request := hostdproto.WorkerControlRequest{Operation: "start", WorkerID: "runtime-test", Executable: path, Version: "test", APIMin: 1, APIMax: 1}
	helperCtx, cancelHelper := context.WithCancel(context.Background())
	for i := 0; i < 2; i++ {
		if _, err := registry.HandleWorkerControl(helperCtx, request); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range []string{"ready", "ready", "activate", "activate"} {
		if _, err := registry.HandleWorkerControl(helperCtx, hostdproto.WorkerControlRequest{Operation: op, WorkerID: request.WorkerID}); err != nil {
			t.Fatal(err)
		}
	}
	cancelHelper()
	if worker.ctx.Err() != nil || starts != 1 || worker.readyCalls != 1 || worker.activeCalls != 1 {
		t.Fatalf("ownership/retries starts=%d ready=%d active=%d context=%v", starts, worker.readyCalls, worker.activeCalls, worker.ctx.Err())
	}
	conflict := request
	conflict.Version = "different"
	if _, err := registry.HandleWorkerControl(ownerCtx, conflict); !errors.Is(err, hostdproto.ErrFenced) {
		t.Fatalf("conflicting launch=%v", err)
	}
	request.WorkerID = "unowned"
	request.Executable = filepath.Join(filepath.Dir(path), "arbitrary")
	if _, err := registry.HandleWorkerControl(ownerCtx, request); !errors.Is(err, hostdproto.ErrInvalidConfig) {
		t.Fatalf("arbitrary path=%v", err)
	}
	worker.stopErr = errors.New("not confirmed gone")
	if _, err := registry.HandleWorkerControl(ownerCtx, hostdproto.WorkerControlRequest{Operation: "stop_active"}); err == nil || registry.active == "" || len(registry.workers) != 1 {
		t.Fatalf("failed stop discarded ownership: %v", err)
	}
	worker.stopErr = nil
	for i := 0; i < 2; i++ {
		if _, err := registry.HandleWorkerControl(ownerCtx, hostdproto.WorkerControlRequest{Operation: "stop_active"}); err != nil {
			t.Fatal(err)
		}
	}
	if registry.active != "" || len(registry.workers) != 0 || worker.stopCalls != 2 {
		t.Fatalf("cleanup active=%s workers=%d stops=%d", registry.active, len(registry.workers), worker.stopCalls)
	}
}
