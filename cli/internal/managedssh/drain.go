package managedssh

import (
	"context"
	"errors"
	"time"
)

// CarrierDrainTimeout bounds how long accepted SSH/rsync bridges may retain a
// retiring transport. At the cap they are closed; clients reconnect normally.
const CarrierDrainTimeout = 30 * time.Second

const carrierDrainCloseTimeout = 5 * time.Second

// CarrierDrainLimit includes forced bridge closure after the courteous drain.
const CarrierDrainLimit = CarrierDrainTimeout + carrierDrainCloseTimeout

func (h *Host) Drain(ctx context.Context) error {
	if h == nil || ctx == nil {
		return ErrSSHHostInvalid
	}
	h.mu.Lock()
	h.retired = true
	if h.changed == nil {
		h.changed = make(chan struct{})
	}
	h.mu.Unlock()
	drain, cancel := context.WithTimeout(ctx, CarrierDrainTimeout)
	defer cancel()
	if err := h.waitDrained(drain); err == nil {
		return nil
	}
	// An aborted update must not terminate accepted sessions. The caller
	// resumes this same primary's admission through the existing rollback.
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
