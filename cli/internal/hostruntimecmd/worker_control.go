//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ownedWorkers retains every initial/candidate/committed feature process in
// native hostd. Only installed slots and the fixed worker entry point exist.
type ownedWorkers struct {
	mu       sync.Mutex
	ctx      context.Context
	start    func(context.Context, workerStartRequest) (workerProcess, error)
	template workerStartRequest
	paths    []string
	workers  map[string]*ownedWorker
	active   string
	lost     chan error
}
type ownedWorker struct {
	request       workerStartRequest
	process       workerProcess
	ready, active hostdproto.Status
	stopping      bool
}

func newOwnedWorkers(ctx context.Context, template workerStartRequest, start func(context.Context, workerStartRequest) (workerProcess, error)) *ownedWorkers {
	return &ownedWorkers{ctx: ctx, start: start, template: template, paths: []string{template.Executable, os.Getenv("PAPERBOAT_BINARY_ROLLBACK"), os.Getenv("PAPERBOAT_BINARY_STAGED")}, workers: map[string]*ownedWorker{}, lost: make(chan error, 1)}
}
func (o *ownedWorkers) validExecutable(path, version string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, allowed := range o.paths {
		if allowed != "" && path == allowed {
			return regularExecutable(path)
		}
	}
	// Windows packages use immutable version slots so committed workers never
	// lock the next canonical installation. Keep the path shape authoritative.
	staged := os.Getenv("PAPERBOAT_BINARY_STAGED")
	if staged != "" && version != "" && filepath.Base(version) == version && !strings.ContainsAny(version, "/\\") {
		expected := filepath.Join(filepath.Dir(staged), "versions", version, filepath.Base(o.template.Executable))
		if path == expected {
			return regularExecutable(path)
		}
	}
	return false
}
func regularExecutable(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}
func (o *ownedWorkers) HandleWorkerControl(ctx context.Context, r hostdproto.WorkerControlRequest) (hostdproto.WorkerControlResponse, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := hostdproto.WorkerControlResponse{Completed: true}
	switch r.Operation {
	case "start":
		if !o.validExecutable(r.Executable, r.Version) {
			return out, hostdproto.ErrInvalidConfig
		}
		if existing := o.workers[r.WorkerID]; existing != nil {
			if existing.request.Executable != r.Executable || existing.request.Version != r.Version || existing.request.APIMin != r.APIMin || existing.request.APIMax != r.APIMax {
				return out, hostdproto.ErrFenced
			}
			return out, nil
		}
		if len(o.workers) >= 2 {
			return out, hostdproto.ErrNotReady
		}
		request := o.template
		request.Executable = r.Executable
		request.WorkerID = r.WorkerID
		request.Version = r.Version
		request.APIMin = r.APIMin
		request.APIMax = r.APIMax
		process, err := o.start(o.ctx, request)
		if err != nil {
			return out, err
		}
		worker := &ownedWorker{request: request, process: process}
		o.workers[r.WorkerID] = worker
		if watcher, ok := process.(exitWatcher); ok {
			go o.watchExit(r.WorkerID, worker, watcher)
		}
		return out, nil
	case "stop_active":
		if o.active == "" {
			return out, nil
		}
		return out, o.stopLocked(ctx, o.active)
	case "stop":
		return out, o.stopLocked(ctx, r.WorkerID)
	case "ready", "activate":
		worker := o.workers[r.WorkerID]
		if worker == nil {
			return out, hostdproto.ErrNotReady
		}
		if r.Operation == "ready" {
			if worker.ready.Epoch == 0 {
				status, err := worker.process.Ready(ctx)
				if err != nil {
					return out, runtimeFailure("runtime worker readiness failed", err, o.stopLocked(context.WithoutCancel(ctx), r.WorkerID))
				}
				worker.ready = status
			}
			out.Status = &worker.ready
			return out, nil
		}
		if worker.active.Epoch == 0 {
			if o.active != "" && o.active != r.WorkerID {
				return out, hostdproto.ErrNotReady
			}
			status, err := worker.process.Activate(ctx)
			if err != nil {
				return out, runtimeFailure("runtime worker activation failed", err, o.stopLocked(context.WithoutCancel(ctx), r.WorkerID))
			}
			worker.active = status
			o.active = r.WorkerID
		}
		out.Status = &worker.active
		return out, nil
	default:
		return out, hostdproto.ErrInvalidFrame
	}
}
func (o *ownedWorkers) stopLocked(ctx context.Context, id string) error {
	worker := o.workers[id]
	if worker == nil {
		return nil
	}
	worker.stopping = true
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := worker.process.Stop(stopCtx); err != nil {
		return err
	}
	delete(o.workers, id)
	if o.active == id {
		o.active = ""
	}
	return nil
}
func (o *ownedWorkers) shutdown(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	var result error
	for id := range o.workers {
		result = errors.Join(result, o.stopLocked(ctx, id))
	}
	return result
}

// watchExit reports the active worker dying without having been stopped, so
// hostd can exit and let its service manager start it with a fresh worker. A
// candidate or deliberately stopped worker is not a failure.
func (o *ownedWorkers) watchExit(id string, worker *ownedWorker, watcher exitWatcher) {
	select {
	case <-watcher.Exited():
	case <-o.ctx.Done():
		return
	}
	o.mu.Lock()
	unexpected := !worker.stopping && o.workers[id] == worker && o.active == id
	o.mu.Unlock()
	if !unexpected || o.ctx.Err() != nil {
		return
	}
	select {
	case o.lost <- runtimeFailure("the runtime worker exited without being stopped: "+watcher.ExitReason(), errWorkerExited):
	default:
	}
}

var errWorkerExited = errors.New("runtime worker exited")
