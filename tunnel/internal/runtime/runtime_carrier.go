package runtime

import (
	"context"
	"errors"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/reporting"
)

// RuntimeCarrierWorker owns the admissions of agents' runtime carriers and their routes: the
// runtime route (Routes) and the endpoint routes riding on the same carrier (Endpoints, the preview
// registry, PLAN 3a). A successful
// complete pull replaces authority; a failed pull preserves admitted carriers until explicit withdrawal.
// Runtime endpoints authenticate the forwarded helper credential themselves.
type RuntimeCarrierWorker struct {
	Reporter           *reporting.Reporter
	Source             control.RuntimeCarrierSource
	Expected           *datacarrier.ExpectedAdmissionRegistry
	Routes             *edgehttp.DataCarrierPreviewRegistry
	Endpoints          *edgehttp.DataCarrierPreviewRegistry
	BrowserTerminalHub *edgehttp.BrowserTerminalHub
	Interval           time.Duration
	// EndpointInterval paces the small endpoint-only pull (PLAN 3a) that admits a new preview or
	// tunnel route within a second; zero disables it (endpoint routes then follow full pulls).
	EndpointInterval time.Duration
	Timeout          time.Duration
	// runtimeSet and endpointSet are the latest runtime and endpoint admissions; only the worker
	// goroutine touches them. endpointPause stops fast pulls until the next full pull after one
	// fails (for example on a server that does not serve them yet).
	runtimeSet    []runtimewire.Admission
	endpointSet   []runtimewire.Admission
	endpointPause bool
	mu            sync.Mutex
	cancel        context.CancelFunc
	done          chan struct{}
	wake          chan struct{}
	closed        bool
}

func (w *RuntimeCarrierWorker) Start(ctx context.Context) error {
	if ctx == nil || w.Source == nil || w.Expected == nil || w.Routes == nil || w.Endpoints == nil || w.Interval <= 0 || w.Timeout <= 0 || w.Interval > 5*time.Minute || w.Timeout > time.Minute {
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
		var fast <-chan time.Time
		if w.EndpointInterval > 0 {
			fastTicker := time.NewTicker(w.EndpointInterval)
			defer fastTicker.Stop()
			fast = fastTicker.C
		}
		for {
			select {
			case <-run.Done():
				return
			case <-ticker.C:
				w.reconcile(run)
			case <-fast:
				w.reconcileEndpoints(run)
			case <-w.wake:
				w.observe(run)
			}
		}
	}()
	return nil
}

// registry is where an admission's route lives.
func (w *RuntimeCarrierWorker) registry(a datacarrier.ExpectedAdmission) *edgehttp.DataCarrierPreviewRegistry {
	if a.EndpointRoute() {
		return w.Endpoints
	}
	return w.Routes
}

// routes is every route this worker owns.
func (w *RuntimeCarrierWorker) routes() []edgehttp.DataCarrierPreviewRoute {
	out := w.Routes.Snapshot()
	for _, r := range w.Endpoints.Snapshot() {
		if r.EndpointRoute {
			out = append(out, r)
		}
	}
	return out
}

func (w *RuntimeCarrierWorker) detachRoute(r edgehttp.DataCarrierPreviewRoute) {
	if r.EndpointRoute {
		_ = w.Endpoints.Detach(r.RouteID, r.Identity, r.Revision)
		return
	}
	_ = w.Routes.Detach(r.RouteID, r.Identity, r.Revision)
	if w.BrowserTerminalHub != nil {
		w.BrowserTerminalHub.DeactivateRoute(r.RouteID, edgehttp.BrowserTerminalRouteFence{Identity: r.Identity, Revision: r.Revision, AttachmentGeneration: r.AttachmentGeneration})
	}
}

func (w *RuntimeCarrierWorker) detachAdmission(a datacarrier.ExpectedAdmission) {
	_ = w.registry(a).DetachAdmission(a)
	if w.BrowserTerminalHub != nil && !a.EndpointRoute() {
		w.BrowserTerminalHub.DeactivateRoute(a.RouteID, browserTerminalFence(a))
	}
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
		w.runtimeSet, w.endpointSet = w.runtimeSet[:0], w.endpointSet[:0]
		for _, value := range values {
			if value.EndpointID != "" {
				w.endpointSet = append(w.endpointSet, value)
			} else {
				w.runtimeSet = append(w.runtimeSet, value)
			}
		}
		w.endpointPause = false
		if err = w.install(); err != nil {
			return err
		}
	}
	w.withdraw()
	w.observe(parent)
	return err
}

// reconcileEndpoints replaces only the endpoint routes, from the small endpoint-only pull. A
// failed pull keeps the last good set and pauses fast pulls until the next full pull.
func (w *RuntimeCarrierWorker) reconcileEndpoints(parent context.Context) (failure error) {
	if w.endpointPause {
		return nil
	}
	defer func() { observeWorkerFailure(parent, w.Reporter, "endpoint_admission", failure) }()
	ctx, cancel := context.WithTimeout(parent, w.Timeout)
	defer cancel()
	values, err := w.Source.EndpointAdmissions(ctx, w.Expected.NodeID(), w.Expected.ProcessEpoch())
	if err != nil {
		w.endpointPause = true
		return err
	}
	if endpointSetEqual(w.endpointSet, values) {
		return nil
	}
	w.endpointSet = append(w.endpointSet[:0], values...)
	if err := w.install(); err != nil {
		return err
	}
	w.withdraw()
	w.observe(parent)
	return nil
}

func endpointSetEqual(a, b []runtimewire.Admission) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Binding != b[i].Binding || a[i].AttachmentGeneration != b[i].AttachmentGeneration || a[i].Hostname != b[i].Hostname || a[i].RouteKind != b[i].RouteKind || a[i].RouteRevision != b[i].RouteRevision || a[i].EndpointID != b[i].EndpointID || a[i].AccessMode != b[i].AccessMode {
			return false
		}
	}
	return true
}

// install makes the expected admissions the latest runtime and endpoint sets.
func (w *RuntimeCarrierWorker) install() error {
	now := time.Now().UTC()
	expected := make([]datacarrier.ExpectedAdmission, 0, len(w.runtimeSet)+len(w.endpointSet))
	for _, set := range [][]runtimewire.Admission{w.runtimeSet, w.endpointSet} {
		for _, value := range set {
			a, err := control.ExpectedRuntimeAdmission(value, w.Expected.NodeID(), w.Expected.ProcessEpoch(), now)
			if err != nil {
				return err
			}
			expected = append(expected, a)
		}
	}
	return w.Expected.Replace(expected, now)
}

// withdraw detaches routes the expected admissions no longer allow. Credential expiry during an
// outage never revokes an admitted carrier: only a successful pull changes the set.
func (w *RuntimeCarrierWorker) withdraw() {
	allowed := map[string]datacarrier.ExpectedAdmission{}
	for _, a := range w.Expected.Snapshot() {
		if a.Admitted {
			allowed[a.RouteID] = a
		}
	}
	for _, r := range w.routes() {
		a, ok := allowed[r.RouteID]
		if !ok || a.Identity != r.Identity || a.AttachmentGeneration != r.AttachmentGeneration || a.RouteRevision != r.Revision {
			w.detachRoute(r)
		}
	}
}

func (w *RuntimeCarrierWorker) observe(parent context.Context) (failure error) {
	defer func() { observeWorkerFailure(parent, w.Reporter, "runtime_observation", failure) }()
	observations := make([]runtimewire.Observation, 0)
	for _, a := range w.Expected.Snapshot() {
		r, ok := w.registry(a).Route(a.RouteID)
		ready := ok && r.Identity == a.Identity && r.AttachmentGeneration == a.AttachmentGeneration && r.Revision == a.RouteRevision
		if ready {
			select {
			case <-r.Server.Done():
				ready = false
			default:
			}
		}
		observations = append(observations, runtimewire.Observation{RouteID: a.RouteID, SessionID: a.Identity.SessionID, AttachmentGeneration: a.AttachmentGeneration, Ready: ready})
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
			w.detachAdmission(a)
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
					if err := w.registry(next).AttachAdmission(next, server); err == nil {
						attached[key] = next
						continue
					}
				}
			}
			if !ok || !previewCarrierAdmissionEqual(a, next) {
				w.detachAdmission(a)
				delete(attached, key)
				changed = true
			}
		}
		for key, a := range desired {
			if _, ok := attached[key]; !ok {
				if err := w.registry(a).AttachAdmission(a, server); err == nil {
					if w.BrowserTerminalHub != nil && !a.EndpointRoute() {
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
	// The endpoint registry is shared with the preview worker: detach only this worker's routes.
	for _, r := range w.Endpoints.Snapshot() {
		if r.EndpointRoute {
			_ = w.Endpoints.Detach(r.RouteID, r.Identity, r.Revision)
		}
	}
	err := w.Routes.Close()
	if w.BrowserTerminalHub != nil {
		err = errors.Join(err, w.BrowserTerminalHub.Close())
	}
	observations := make([]runtimewire.Observation, 0, len(admissions))
	for _, a := range admissions {
		observations = append(observations, runtimewire.Observation{RouteID: a.RouteID, SessionID: a.Identity.SessionID, AttachmentGeneration: a.AttachmentGeneration})
	}
	observeCtx, cancelObserve := context.WithTimeout(ctx, w.Timeout)
	defer cancelObserve()
	return errors.Join(err, w.Source.ObserveRuntimeCarriers(observeCtx, w.Expected.NodeID(), w.Expected.ProcessEpoch(), observations))
}

func browserTerminalFence(admission datacarrier.ExpectedAdmission) edgehttp.BrowserTerminalRouteFence {
	return edgehttp.BrowserTerminalRouteFence{Identity: admission.Identity, Revision: admission.RouteRevision, AttachmentGeneration: admission.AttachmentGeneration}
}
