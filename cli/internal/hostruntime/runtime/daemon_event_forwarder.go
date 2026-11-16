//go:build darwin || linux || windows

package runtime

import (
	"context"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type daemonEventForwarder struct {
	hub     *machineevents.Hub
	events  *machineevents.Subscription
	machine string
	push    func(context.Context, localapi.MachineEventPush) error
	cancel  context.CancelFunc
	done    chan struct{}
}

func newDaemonEventForwarder(hub *machineevents.Hub, machine string, push func(context.Context, localapi.MachineEventPush) error) *daemonEventForwarder {
	return &daemonEventForwarder{hub: hub, events: hub.Subscribe(machineevents.DaemonTopics...), machine: machine, push: push}
}

func pushDaemonEvents(ctx context.Context, push localapi.MachineEventPush) error {
	paths, err := localapi.CurrentPaths(os.Geteuid())
	if err != nil {
		return err
	}
	client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
	if err != nil {
		return err
	}
	return client.PushMachineEvents(ctx, push)
}

func (f *daemonEventForwarder) Start(ctx context.Context) error {
	ctx, f.cancel = context.WithCancel(ctx)
	f.done = make(chan struct{})
	go func() {
		defer close(f.done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		sent := make(map[machineevents.Topic]int64)
		var invalidations uint64
		for {
			known, currentInvalidations := f.hub.Generations()
			push := localapi.MachineEventPush{MachineID: f.machine, Healthy: f.hub.Healthy()}
			for _, topic := range machineevents.DaemonTopics {
				if known[topic] > sent[topic] {
					push.Topics = append(push.Topics, topic)
				}
			}
			// Recovery and authorization failures wake all worker subscriptions
			// without moving a cursor. Preserve those invalidations too.
			if currentInvalidations != invalidations {
				push.Topics = append([]machineevents.Topic(nil), machineevents.DaemonTopics...)
			}
			// On a failed push retain the invalidations for the next heartbeat.
			if f.push(ctx, push) == nil {
				sent, invalidations = known, currentInvalidations
			}
			select {
			case <-ctx.Done():
				return
			case <-f.events.Changed():
			case <-ticker.C:
			}
		}
	}()
	return nil
}

func (f *daemonEventForwarder) Shutdown(ctx context.Context) error {
	if f.cancel == nil {
		return nil
	}
	f.cancel()
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
