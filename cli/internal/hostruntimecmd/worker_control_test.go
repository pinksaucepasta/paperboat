//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	registry := newOwnedWorkers(ownerCtx, workerStartRequest{Executable: path, MutationsDisabled: true}, func(ctx context.Context, r workerStartRequest) (workerProcess, error) {
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

type dyingWorker struct {
	registryWorker
	exited chan struct{}
}

func (w *dyingWorker) Exited() <-chan struct{} { return w.exited }
func (w *dyingWorker) ExitReason() string      { return "signal: killed; last output: boom" }
func (w *dyingWorker) Stop(context.Context) error {
	return nil
}

// Device finding: the HP runtime worker died around a server restart and hostd,
// which never waited on it, did not notice or restart it.
func TestOwnedWorkersReportAnActiveWorkerThatExitsUnstopped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	start := func(workerID string) (*ownedWorkers, *dyingWorker) {
		worker := &dyingWorker{exited: make(chan struct{})}
		registry := newOwnedWorkers(ownerCtx, workerStartRequest{Executable: path, MutationsDisabled: true}, func(context.Context, workerStartRequest) (workerProcess, error) { return worker, nil })
		for _, op := range []string{"start", "ready", "activate"} {
			if _, err := registry.HandleWorkerControl(ownerCtx, hostdproto.WorkerControlRequest{Operation: op, WorkerID: workerID, Executable: path, Version: "test", APIMin: 1, APIMax: 1}); err != nil {
				t.Fatal(err)
			}
		}
		return registry, worker
	}
	registry, worker := start("runtime-test")
	close(worker.exited)
	select {
	case err := <-registry.lost:
		if !errors.Is(err, errWorkerExited) || !strings.Contains(err.Error(), "last output: boom") {
			t.Fatalf("lost=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hostd was not told that its active worker exited")
	}

	registry, worker = start("runtime-test")
	if _, err := registry.HandleWorkerControl(ownerCtx, hostdproto.WorkerControlRequest{Operation: "stop", WorkerID: "runtime-test"}); err != nil {
		t.Fatal(err)
	}
	close(worker.exited)
	select {
	case err := <-registry.lost:
		t.Fatalf("a deliberate stop was reported as a failure: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}
