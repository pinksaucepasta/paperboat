//go:build darwin || linux || windows

package runtime

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"time"
)

// carrierEventForwarder shares the worker stream with its independent carrier.
// Failed local delivery is retried on the next heartbeat; the carrier's health
// lease expires if this worker dies. No delivery blocks a worker event consumer.
type carrierEventForwarder struct {
	recovered chan struct{}
	hub       *machineevents.Hub
	events    *machineevents.Subscription
	push      func(context.Context, bool, bool) error
	cancel    context.CancelFunc
	done      chan struct{}
}

func newCarrierEventForwarder(hub *machineevents.Hub, push func(context.Context, bool, bool) error) *carrierEventForwarder {
	return &carrierEventForwarder{hub: hub, events: hub.Subscribe(carrierRenewalTopics...), push: push, recovered: make(chan struct{}, 1)}
}

func (f *carrierEventForwarder) Start(ctx context.Context) error {
	ctx, f.cancel = context.WithCancel(ctx)
	f.done = make(chan struct{})
	go func() {
		defer close(f.done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		refresh := true // covers events missed while the worker was absent
		recovery := f.hub.Recovered()
		_ = f.push(ctx, refresh, f.hub.Healthy())
		for {
			select {
			case <-ctx.Done():
				return
			case <-recovery:
				select {
				case f.recovered <- struct{}{}:
				default:
				}
				_ = f.push(ctx, false, false) // force recovery even after a brief outage between heartbeats
				refresh = true
			case <-f.events.Changed():
				refresh = true
			case <-ticker.C:
			}
			if err := f.push(ctx, refresh, f.hub.Healthy()); err == nil {
				refresh = false
			}
		}
	}()
	return nil
}

func (f *carrierEventForwarder) Shutdown(ctx context.Context) error {
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
