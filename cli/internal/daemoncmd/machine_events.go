package daemoncmd

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type daemonMachineEvents struct {
	machine string
	hub     *machineevents.Hub
	remote  bool
	mu      sync.Mutex
}

// A worker descriptor is written before serving. While an installed worker is
// stopped, subscriptions keep their normal timers rather than adding a wait.
func daemonHasWorker() bool {
	root := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	if root == "" {
		var err error
		root, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return false
		}
	}
	_, err := os.Lstat(filepath.Join(root, "runtime", "worker-local.json"))
	return !os.IsNotExist(err)
}

func newDaemonMachineEvents(machine string, worker bool, wait machineevents.WaitFunc) (*daemonMachineEvents, error) {
	events := &daemonMachineEvents{machine: machine, hub: machineevents.NewPushed()}
	if machine != "" && !worker {
		var err error
		events.hub, err = machineevents.NewWithWaiter(wait, 0)
		events.remote = true
		if err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (e *daemonMachineEvents) start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.remote {
		return nil
	}
	return e.hub.Start(ctx)
}

func (e *daemonMachineEvents) receive(ctx context.Context, push localapi.MachineEventPush) error {
	if push.MachineID != e.machine {
		return localapi.ErrInvalidConfig
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// A worker starting after this daemon takes over immediately. Stop the
	// credential stream before delivering its first local push.
	if e.remote {
		if err := e.hub.Shutdown(ctx); err != nil {
			return err
		}
		e.remote = false
	}
	e.hub.Push(push.Topics, push.Healthy)
	return nil
}

func (e *daemonMachineEvents) stop(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.remote {
		_ = e.hub.Shutdown(ctx)
	}
}
