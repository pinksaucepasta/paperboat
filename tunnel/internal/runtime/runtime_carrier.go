package runtime

import (
	"context"
	"errors"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"sort"
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
	// EndpointFullInterval: how often the endpoint pull takes the full set instead of deltas (zero:
	// endpointFullInterval, the server's admission lifetime).
	EndpointFullInterval time.Duration
	Timeout              time.Duration
	// runtimeSet and endpointSet are the latest runtime and endpoint admissions; only the worker
	// goroutine touches them. endpointPause stops fast pulls until the next full pull after one
	// fails (for example on a server that does not serve them yet).
	runtimeSet    []runtimewire.Admission
	endpointSet   []runtimewire.Admission
	endpointPause bool
	// endpointCursor is the server's cursor of the last endpoint answer; endpointFullAt when the last
	// full endpoint set arrived; endpointExpiry the expiry the installed endpoint admissions carry.
	endpointCursor string
	endpointFullAt time.Time
	endpointExpiry time.Time
	mu             sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan struct{}
	closed         bool
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
		// Endpoint routes come only from the endpoint pull (endpoint deltas); this one is runtime.
		w.runtimeSet = w.runtimeSet[:0]
		for _, value := range values {
			if value.EndpointID == "" {
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

// endpointFullInterval is how often the endpoint pull asks for the full set instead of deltas: the
// safety net for changes no delta reports (a lease lapsing, a carrier going stale). It equals the
// server's admission lifetime, which every delta renews.
const endpointFullInterval = 2 * time.Minute

// endpointRenewStep: a delta's later expiry is installed once it moved this far, not on every poll.
const endpointRenewStep = 30 * time.Second

// reconcileEndpoints updates only the endpoint routes from the endpoint pull: the full set when due,
// otherwise the changes since the last cursor. A failed pull keeps the last good set and pauses
// fast pulls until the next full runtime pull.
func (w *RuntimeCarrierWorker) reconcileEndpoints(parent context.Context) (failure error) {
	if w.endpointPause {
		return nil
	}
	defer func() { observeWorkerFailure(parent, w.Reporter, "endpoint_admission", failure) }()
	ctx, cancel := context.WithTimeout(parent, w.Timeout)
	defer cancel()
	since, full := w.endpointCursor, w.EndpointFullInterval
	if full <= 0 {
		full = endpointFullInterval
	}
	if time.Since(w.endpointFullAt) >= full {
		since = ""
	}
	update, err := w.Source.EndpointAdmissions(ctx, w.Expected.NodeID(), w.Expected.ProcessEpoch(), since)
	if err != nil {
		w.endpointPause = true
		return err
	}
	next, expiry := mergeEndpointUpdate(w.endpointSet, update)
	if update.Complete {
		w.endpointFullAt = time.Now()
	}
	w.endpointCursor = update.Cursor
	changed := !endpointSetEqual(w.endpointSet, next)
	if !changed && expiry.Sub(w.endpointExpiry) < endpointRenewStep {
		return nil
	}
	w.endpointSet, w.endpointExpiry = next, expiry
	if err := w.install(); err != nil {
		return err
	}
	if changed {
		w.withdraw()
		w.observe(parent)
	}
	return nil
}

// mergeEndpointUpdate applies a full set or a delta to the current endpoint admissions (sorted by
// route id) and returns the result with the expiry its admissions carry.
func mergeEndpointUpdate(current []runtimewire.Admission, update control.EndpointUpdate) ([]runtimewire.Admission, time.Time) {
	byRoute := map[string]runtimewire.Admission{}
	if !update.Complete {
		for _, a := range current {
			byRoute[a.Binding.RouteID] = a
		}
	}
	for _, id := range update.Removed {
		delete(byRoute, id)
	}
	for _, a := range update.Admissions {
		byRoute[a.Binding.RouteID] = a
	}
	next := make([]runtimewire.Admission, 0, len(byRoute))
	var expiry time.Time
	for _, a := range byRoute {
		if !update.Complete {
			a.ExpiresAt = update.ExpiresAt
		}
		if expiry.IsZero() || a.ExpiresAt.Before(expiry) {
			expiry = a.ExpiresAt
		}
		next = append(next, a)
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Binding.RouteID < next[j].Binding.RouteID })
	return next, expiry
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
	for {
		// Woken by a change of the expected admissions or the next expiry, not polled (audit T2).
		changes := w.Expected.Changed()
		desired := map[string]datacarrier.ExpectedAdmission{}
		var nextExpiry time.Time
		for _, a := range w.Expected.ForIdentity(server.Identity(), time.Now().UTC()) {
			desired[a.RouteID] = a
			// Only a future expiry is a reason to wake; a past one stays until the next change.
			if a.ExpiresAt.After(time.Now()) && (nextExpiry.IsZero() || a.ExpiresAt.Before(nextExpiry)) {
				nextExpiry = a.ExpiresAt
			}
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
		wait := handlerRecheck
		if !nextExpiry.IsZero() {
			wait = min(max(time.Until(nextExpiry), handlerMinimumWait), handlerRecheck)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-server.Done():
			timer.Stop()
			return nil
		case <-changes:
		case <-timer.C:
		case <-server.Draining():
			// The host is going away (T10): its routes leave this edge now, so new requests get
			// the route-unavailable answer instead of a carrier that is closing; open streams
			// finish. A carrier still open when the goaway ends is in service again and its
			// routes come back on the next pass.
			timer.Stop()
			for key, a := range attached {
				w.detachAdmission(a)
				delete(attached, key)
			}
			w.changed()
			drained := time.NewTimer(max(time.Until(server.DrainUntil()), 0))
			select {
			case <-ctx.Done():
				drained.Stop()
				return ctx.Err()
			case <-server.Done():
				drained.Stop()
				return nil
			case <-drained.C:
				server.EndDrain(time.Now())
			}
		case err := <-outputErrors:
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// A carrier handler rechecks its admissions on a change, at the next expiry (not sooner than
// handlerMinimumWait), and at least every handlerRecheck.
const (
	handlerMinimumWait = 100 * time.Millisecond
	handlerRecheck     = 30 * time.Second
)

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
