package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
)

// RuntimeCarrierWorker owns runtime-only admissions and routes. A successful
// complete pull replaces authority; a failed pull retains it only until expiry.
// Runtime endpoints authenticate the forwarded helper credential themselves.
type RuntimeCarrierWorker struct {
	Reporter           *reporting.Reporter
	Source             control.RuntimeCarrierSource
	Expected           *datacarrier.ExpectedAdmissionRegistry
	Routes             *edgehttp.DataCarrierPreviewRegistry
	BrowserTerminalHub *edgehttp.BrowserTerminalHub
	Interval           time.Duration
	Timeout            time.Duration
	mu                 sync.Mutex
	cancel             context.CancelFunc
	done               chan struct{}
	wake               chan struct{}
	closed             bool
}

func (w *RuntimeCarrierWorker) Start(ctx context.Context) error {
	if ctx == nil || w.Source == nil || w.Expected == nil || w.Routes == nil || w.Interval <= 0 || w.Timeout <= 0 || w.Interval > 5*time.Minute || w.Timeout > time.Minute {
		return ErrProcessInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil || w.closed {
		return ErrProcessInvalid
	}
	run, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = make(chan struct{})
	w.wake = make(chan struct{}, 1)
	go func() {
		defer close(w.done)
		w.reconcile(run)
		ticker := time.NewTicker(w.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-run.Done():
				return
			case <-ticker.C:
				w.reconcile(run)
			case <-w.wake:
				w.observe(run)
			}
		}
	}()
	return nil
}

func (w *RuntimeCarrierWorker) changed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wake != nil {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

func (w *RuntimeCarrierWorker) reconcile(parent context.Context) (failure error) {
	defer func() { observeWorkerFailure(parent, w.Reporter, "runtime_admission", failure) }()
	ctx, cancel := context.WithTimeout(parent, w.Timeout)
	defer cancel()
	values, err := w.Source.RuntimeCarrierAdmissions(ctx, w.Expected.NodeID(), w.Expected.ProcessEpoch())
	if err == nil {
		expected := make([]datacarrier.ExpectedAdmission, 0, len(values))
		for _, value := range values {
			a, e := value.Expected(w.Expected.NodeID(), w.Expected.ProcessEpoch(), time.Now().UTC())
			if e != nil {
				return e
			}
			expected = append(expected, a)
		}
		if err = w.Expected.Replace(expected, time.Now().UTC()); err != nil {
			return err
		}
	}
	// Even control failures cannot extend a lease or keep a revoked local route.
	allowed := map[string]datacarrier.ExpectedAdmission{}
	for _, a := range w.Expected.Snapshot() {
		if a.ExpiresAt.After(time.Now().UTC()) {
			allowed[a.RouteID] = a
		}
	}
	for _, r := range w.Routes.Snapshot() {
		a, ok := allowed[r.RouteID]
		if !ok || a.Identity != r.Identity || a.AttachmentGeneration != r.AttachmentGeneration || a.RouteRevision != r.Revision {
			_ = w.Routes.Detach(r.RouteID, r.Identity, r.Revision)
			if w.BrowserTerminalHub != nil {
				w.BrowserTerminalHub.DeactivateRoute(r.RouteID, edgehttp.BrowserTerminalRouteFence{Identity: r.Identity, Revision: r.Revision, AttachmentGeneration: r.AttachmentGeneration})
			}
		}
	}
	w.observe(parent)
	return err
}

func (w *RuntimeCarrierWorker) observe(parent context.Context) (failure error) {
	defer func() { observeWorkerFailure(parent, w.Reporter, "runtime_observation", failure) }()
	observations := make([]control.RuntimeCarrierObservation, 0)
	for _, a := range w.Expected.Snapshot() {
		r, ok := w.Routes.Route(a.RouteID)
		ready := ok && r.Identity == a.Identity && r.AttachmentGeneration == a.AttachmentGeneration && r.Revision == a.RouteRevision && a.ExpiresAt.After(time.Now().UTC())
		if ready {
			select {
			case <-r.Server.Done():
				ready = false
			default:
			}
		}
		observations = append(observations, control.RuntimeCarrierObservation{RouteID: a.RouteID, SessionID: a.Identity.SessionID, AttachmentGeneration: a.AttachmentGeneration, Ready: ready})
	}
	ctx, cancel := context.WithTimeout(parent, w.Timeout)
	defer cancel()
	return w.Source.ObserveRuntimeCarriers(ctx, w.Expected.NodeID(), w.Expected.ProcessEpoch(), observations)
}

func (w *RuntimeCarrierWorker) Handle(ctx context.Context, server *datacarrier.Server) error {
	if ctx == nil || server == nil {
		return ErrProcessInvalid
	}
	attached := map[string]datacarrier.ExpectedAdmission{}
	var outputErrors chan error
	defer func() {
		for _, a := range attached {
			_ = w.Routes.DetachAdmission(a)
			if w.BrowserTerminalHub != nil {
				w.BrowserTerminalHub.DeactivateRoute(a.RouteID, browserTerminalFence(a))
			}
		}
		w.changed()
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		desired := map[string]datacarrier.ExpectedAdmission{}
		for _, a := range w.Expected.ForIdentity(server.Identity(), time.Now().UTC()) {
			desired[a.RouteID] = a
		}
		changed := false
		for key, a := range attached {
			next, ok := desired[key]
			if ok {
				previous := a
				previous.ExpiresAt = next.ExpiresAt
				if previewCarrierAdmissionEqual(previous, next) {
					if err := w.Routes.AttachAdmission(next, server); err == nil {
						attached[key] = next
						continue
					}
				}
			}
			if !ok || !previewCarrierAdmissionEqual(a, next) {
				_ = w.Routes.DetachAdmission(a)
				if w.BrowserTerminalHub != nil {
					w.BrowserTerminalHub.DeactivateRoute(a.RouteID, browserTerminalFence(a))
				}
				delete(attached, key)
				changed = true
			}
		}
		for key, a := range desired {
			if _, ok := attached[key]; !ok {
				if err := w.Routes.AttachAdmission(a, server); err == nil {
					if w.BrowserTerminalHub != nil {
						if err := w.BrowserTerminalHub.ActivateRoute(a.RouteID, browserTerminalFence(a)); err != nil {
							_ = w.Routes.DetachAdmission(a)
							continue
						}
					}
					attached[key] = a
					changed = true
				}
			}
		}
		if w.BrowserTerminalHub != nil && outputErrors == nil {
			outputErrors = make(chan error, 1)
			go func() { outputErrors <- w.BrowserTerminalHub.ServeCarrier(ctx, server) }()
		}
		if changed {
			w.changed()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-server.Done():
			return nil
		case <-ticker.C:
		case err := <-outputErrors:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

func (w *RuntimeCarrierWorker) Shutdown(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Clear authorization first to prevent a live carrier reattaching during stop.
	admissions := w.Expected.Snapshot()
	_ = w.Expected.Close()
	err := w.Routes.Close()
	if w.BrowserTerminalHub != nil {
		err = errors.Join(err, w.BrowserTerminalHub.Close())
	}
	observations := make([]control.RuntimeCarrierObservation, 0, len(admissions))
	for _, a := range admissions {
		observations = append(observations, control.RuntimeCarrierObservation{RouteID: a.RouteID, SessionID: a.Identity.SessionID, AttachmentGeneration: a.AttachmentGeneration})
	}
	observeCtx, cancelObserve := context.WithTimeout(ctx, w.Timeout)
	defer cancelObserve()
	return errors.Join(err, w.Source.ObserveRuntimeCarriers(observeCtx, w.Expected.NodeID(), w.Expected.ProcessEpoch(), observations))
}

func browserTerminalFence(admission datacarrier.ExpectedAdmission) edgehttp.BrowserTerminalRouteFence {
	return edgehttp.BrowserTerminalRouteFence{Identity: admission.Identity, Revision: admission.RouteRevision, AttachmentGeneration: admission.AttachmentGeneration}
}
