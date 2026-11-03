//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

type terminalHandover struct{ previous, candidate *workloadbridge.Client }

// Like nginx's new-worker-before-drain ordering (~/ref/nginx/src/os/unix/
// ngx_process_cycle.c), the staged owner admits clients before primary cutover.
// The signed address offer replaces socket sharing; WG/QUIC crypto is never
// copied into another process (~/ref/tailscale/wgengine/magicsock/magicsock.go).
func stageTerminalHandover(ctx context.Context, r Request, retained CarrierConfig, primary *carriercontrol.Client) (_ *terminalHandover, resultErr error) {
	catalog, next, err := NativeSessionOwnerConfig(ctx, r)
	if err != nil {
		return nil, err
	}
	previous, err := sessionowner.Read(filepath.Join(catalog.Root, retained.Source.SHA256[:32]+".json"))
	if err != nil || previous.Source != retained.Source || previous.RuntimeRoot != r.StateRoot || previous.UID != r.UID || previous.GID != r.GID {
		return nil, errors.Join(ErrInvalidRequest, err)
	}
	h := &terminalHandover{}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, h.finish(context.WithoutCancel(ctx), true))
		}
	}()
	h.previous, err = sessionowner.Connect(ctx, previous)
	if err != nil {
		return nil, err
	}
	state, err := h.previous.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "status"})
	if err != nil {
		return nil, err
	}
	h.candidate, err = sessionowner.Connect(ctx, next)
	if err != nil {
		return nil, err
	}
	slot := 1
	if state.Slot == slot {
		slot = 2
	}
	ready, err := h.candidate.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "start", Slot: slot})
	if err != nil || ready.Slot != slot || len(ready.Offer) == 0 {
		return nil, errors.Join(ErrInvalidRequest, err)
	}
	// Independent old primary and delegated clients prepare in parallel. A
	// declined move leaves that client on today's reconnect path at cutover;
	// already moved clients keep the new generation, even on a partial decline.
	results := make(chan error, 2)
	go func() { results <- primary.MoveTerminals(ctx, ready.Offer) }()
	go func() {
		_, err := h.previous.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "move", Offer: ready.Offer})
		results <- err
	}()
	for range 2 {
		if err := <-results; err != nil {
			diagnosticlog.TryWarn("terminal handover declined", "fallback", "reconnect")
		}
	}
	return h, nil
}

func (h *terminalHandover) finish(ctx context.Context, failed bool) error {
	if h == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var result error
	if h.candidate != nil {
		if failed {
			_, result = h.candidate.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "stop"})
		} else {
			_, result = h.candidate.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "seal"})
		}
		h.candidate.Close()
	}
	if h.previous != nil {
		if !failed {
			_, stopErr := h.previous.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "stop"})
			result = errors.Join(result, stopErr)
		}
		h.previous.Close()
	}
	return result
}
