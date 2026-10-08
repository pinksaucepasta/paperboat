package main

import (
	"context"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

// A terminal attachment owns one inbox poller. Replacement and terminal exit
// cancel and join that poller before releasing its transfer connection.
type terminalInboxWorker struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *terminalInboxWorker) start(parent context.Context, run func(context.Context) error, failed func(error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
	if run == nil || parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel, w.done = cancel, make(chan struct{})
	done := w.done
	go func() {
		defer close(done)
		if err := run(ctx); err != nil {
			failed(err)
		}
	}()
}

func (w *terminalInboxWorker) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
}

func (w *terminalInboxWorker) stopLocked() {
	if w.cancel != nil {
		w.cancel()
		<-w.done
		w.cancel, w.done = nil, nil
	}
}

func reportTerminalAuxFailure(ctx context.Context, stage, code string, err error) string {
	if err == nil {
		return ""
	}
	kind := classifyCommandFailure(err).kind
	projected := errorreport.ProjectFault(ctx, "pb", "connect", stage, code, err)
	if kind == commandCanceled || kind == commandInteractiveCanceled || projected.Outcome == "canceled" {
		return ""
	}
	if errorreport.HTTPAttemptObserved(err) {
		return projected.SupportReference
	}
	if kind == commandUnexpected {
		return errorreport.Current().CaptureFailure(ctx, "pb", "connect", stage, code, err).SupportReference
	}
	return errorreport.Current().ObserveFailure(ctx, "pb", "connect", stage, code, err).SupportReference
}
