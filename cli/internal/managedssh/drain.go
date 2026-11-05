package managedssh

import (
	"context"
	"errors"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

const carrierDrainCloseTimeout = 5 * time.Second

// Drain fences new bridges at once, then waits for every accepted one to end,
// closing those still open at runtimewire.DrainLimit. A cancelled ctx returns
// without closing anything.
func (h *Host) Drain(ctx context.Context) error { return h.drain(ctx, runtimewire.DrainLimit) }

func (h *Host) drain(ctx context.Context, timeout time.Duration) error {
	if h == nil || ctx == nil {
		return ErrSSHHostInvalid
	}
	h.mu.Lock()
	h.retired = true
	if h.changed == nil {
		h.changed = make(chan struct{})
	}
	h.mu.Unlock()
	drain, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := h.waitDrained(drain); err == nil {
		return nil
	}
	// A stopped carrier must not terminate accepted sessions itself; its exit
	// does, and an aborted retirement may still resume admission.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	h.mu.RLock()
	flows := make([]*hostFlow, 0, len(h.flows))
	for flow := range h.flows {
		flows = append(flows, flow)
	}
	h.mu.RUnlock()
	for _, flow := range flows {
		flow.cancel()
	}
	closing, stop := context.WithTimeout(context.WithoutCancel(ctx), carrierDrainCloseTimeout)
	defer stop()
	if err := h.waitDrained(closing); err != nil {
		return errors.Join(ErrSSHSessionsOpen, err)
	}
	return nil
}

func (h *Host) waitDrained(ctx context.Context) error {
	for {
		h.mu.RLock()
		empty, changed := len(h.flows) == 0, h.changed
		h.mu.RUnlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
