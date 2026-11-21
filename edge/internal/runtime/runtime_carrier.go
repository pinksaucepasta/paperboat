package runtime

import (
	"context"
	"errors"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	sharedtelemetry "github.com/pinksaucepasta/paperboat/shared/telemetry"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
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
	// EndpointInterval is the pause before the first held endpoint pull, the shortest gap after a pull that
	// answered at once with nothing new, and the pause between checks while paused after a failure; zero disables the endpoint
	// pull (endpoint routes then follow full pulls). Once the worker holds a cursor the pull is held at the
	// server (EndpointWait) and answered the moment a route is written, so a new preview or tunnel route
	// reaches every edge together (PLAN 3a).
	EndpointInterval time.Duration
	// EndpointWait is how long the server may hold a delta pull with nothing new (zero: endpointWait).
	EndpointWait time.Duration
	// EndpointFullInterval: how often the endpoint pull takes the full set instead of deltas (zero:
	// endpointFullInterval, the server's admission lifetime).
	EndpointFullInterval time.Duration
	Timeout              time.Duration
	// Ready, when set, holds every pull and report until the server has recorded this process
	// as ready (NodeWorker.Ready): it refuses them from an unready node.
	Ready <-chan struct{}
	// OnTunnelsChanged, when set, runs when an endpoint pull reports a tunnel assignment change this
	// worker has not seen: tunnel routes are pulled by the RouteWorker, which would otherwise wait for
	// its control interval.
	OnTunnelsChanged func()
	tunnelsChangedAt string
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
	// OnPull, when set, is told of the first successful runtime pull and of every change in the
	// number of admissions it returned, so a refused carrier can be placed against what the edge held.
	OnPull func(pull uint64, admissions, previous int)
	// pulls counts successful runtime pulls; pulledCount is the size of the last one (worker goroutine only).
	pulls       uint64
	pulledCount int
	mu          sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	wake        chan struct{}
	// pullWake and pullDone serve RequestPull: pullDone is closed once a pull that began after the
	// request has finished.
	pullWake chan struct{}
	pullDone chan struct{}
	// pulled is closed after the first successful runtime pull and the first successful endpoint pull (Pulled).
	pulled          chan struct{}
	endpointsPulled bool
	closed          bool
}

// Pulled is closed once the first runtime pull AND the first endpoint pull installed this process's admissions
// (previews and endpoint tunnels ride the endpoint pull): a replacement process takes over from the one it
// replaces only after that, so it never denies a route it simply has not loaded yet.
func (w *RuntimeCarrierWorker) Pulled() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pulled == nil {
		w.pulled = make(chan struct{})
	}
	return w.pulled
}

// markPulled closes Pulled once both first pulls are applied. Only the worker's own goroutine calls it.
func (w *RuntimeCarrierWorker) markPulled() {
	if w.pulls == 0 || !w.endpointsPulled {
		return
	}
	w.Pulled() // makes the channel
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.pulled:
	default:
		close(w.pulled)
	}
}

// missPullInterval bounds pulls asked for by carriers that matched no admission, so unadmitted
// peers cannot drive control-plane traffic above one pull a second per edge.
const missPullInterval = time.Second

// RequestPull asks for a runtime admission pull now instead of at the next tick and returns a
// channel closed once a pull that began after this call has finished. Concurrent requests share
// one pull. The server hands a machine its admission for this process up to one tick before
// the edge pulls it (prod 2026-11-05: 22 host carriers refused in the 5 s after a restart).
func (w *RuntimeCarrierWorker) RequestPull() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		done := make(chan struct{})
		close(done)
		return done
	}
	if w.pullDone == nil {
		w.pullDone = make(chan struct{})
		if w.pullWake != nil {
			select {
			case w.pullWake <- struct{}{}:
			default:
			}
		}
	}
	return w.pullDone
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
	w.pullWake = make(chan struct{}, 1)
	if w.pullDone != nil {
		w.pullWake <- struct{}{}
	}
	go func() {
		defer close(w.done)
		if w.Ready != nil {
			select {
			case <-run.Done():
				return
			case <-w.Ready:
			}
		}
		w.reconcile(run, nil)
		w.reconcileEndpoints(run) // the first endpoint set is part of being warm, not a tick away
		var lastMissPull time.Time
		ticker := time.NewTicker(w.Interval)
		defer ticker.Stop()
		// One endpoint pull is in flight at a time and its answer is applied here, so the worker's state stays
		// with this goroutine while the server holds the pull.
		results := make(chan endpointPull, 1)
		fastTimer := time.NewTimer(w.EndpointInterval)
		defer fastTimer.Stop()
		fast := fastTimer.C
		if w.EndpointInterval <= 0 {
			fast = nil
		}
		for {
			select {
			case <-run.Done():
				return
			case <-ticker.C:
				w.reconcile(run, nil)
			case <-fast:
				if w.endpointPause { // a failed pull waits for the next runtime pull
					fastTimer.Reset(w.EndpointInterval)
					continue
				}
				since, wait := w.nextEndpointPull()
				go func() { results <- w.pullEndpoints(run, since, wait) }()
			case pull := <-results:
				fastTimer.Reset(w.applyEndpointPull(run, pull))
			case <-w.wake:
				w.observe(run)
			case <-w.pullWake:
				if wait := time.Until(lastMissPull.Add(missPullInterval)); wait > 0 {
					select {
					case <-run.Done():
						return
					case <-time.After(wait):
					}
				}
				w.mu.Lock()
				done := w.pullDone
				w.pullDone = nil
				w.mu.Unlock()
				lastMissPull = time.Now()
				w.reconcile(run, func() {
					if done != nil {
						close(done)
					}
				})
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

func (w *RuntimeCarrierWorker) detachRoute(r edgehttp.DataCarrierPreviewRoute, log *removalLog, reason string) {
	log.add(r.RouteID, r.PreviewID, reason)
	if r.EndpointRoute {
		_ = w.Endpoints.Detach(r.RouteID, r.Identity, r.Revision)
		return
	}
	_ = w.Routes.Detach(r.RouteID, r.Identity, r.Revision)
	if w.BrowserTerminalHub != nil {
		w.BrowserTerminalHub.DeactivateRoute(r.RouteID, edgehttp.BrowserTerminalRouteFence{Identity: r.Identity, Revision: r.Revision, AttachmentGeneration: r.AttachmentGeneration})
	}
}

func (w *RuntimeCarrierWorker) detachAdmission(a datacarrier.ExpectedAdmission, log *removalLog, reason string) {
	log.add(a.RouteID, a.PreviewID, reason)
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

// reconcile pulls the runtime admissions and installs them. pulled (if any) runs once the admissions are
// installed, or the pull failed, before the withdrawals and the observation report that follow: a carrier
// handshake waiting for this pull must not also wait for a control plane that answers the report slowly
// (a replacement server's first requests; gated deploys 2026-11-13 refused machines unknown_machine on it).
func (w *RuntimeCarrierWorker) reconcile(parent context.Context, pulled func()) (failure error) {
	defer func() { observeWorkerFailure(parent, w.Reporter, "runtime_admission", failure) }()
	release := func() {}
	if pulled != nil {
		release = sync.OnceFunc(pulled)
		defer release()
	}
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
		w.pulls++
		w.markPulled()
		if w.OnPull != nil && (w.pulls == 1 || len(w.runtimeSet) != w.pulledCount) {
			w.OnPull(w.pulls, len(w.runtimeSet), w.pulledCount)
		}
		w.pulledCount = len(w.runtimeSet)
	}
	release()
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

// endpointWait is how long the server may hold a delta endpoint pull that has nothing new: below the server's
// bound (runtimewire.MaximumEndpointWaitMilliseconds) and long enough that an idle edge makes ~2 requests a minute.
const endpointWait = 25 * time.Second

// endpointPull is the answer to one endpoint pull and how long it took.
type endpointPull struct {
	update  control.EndpointUpdate
	err     error
	elapsed time.Duration
	done    time.Time
}

// nextEndpointPull is what the next endpoint pull asks for: the cursor to continue from ("" when the full set
// is due, which the server answers at once) and how long the server may hold it.
func (w *RuntimeCarrierWorker) nextEndpointPull() (since string, wait time.Duration) {
	since, full := w.endpointCursor, w.EndpointFullInterval
	if full <= 0 {
		full = endpointFullInterval
	}
	if time.Since(w.endpointFullAt) >= full {
		since = ""
	}
	if since == "" {
		return "", 0
	}
	if wait = w.EndpointWait; wait <= 0 {
		wait = endpointWait
	}
	return since, wait
}

// pullEndpoints asks the server for the endpoint routes; the request is bounded by the wait plus the control
// timeout. It reads no worker state, so it may run beside the worker's loop.
func (w *RuntimeCarrierWorker) pullEndpoints(parent context.Context, since string, wait time.Duration) endpointPull {
	ctx, cancel := context.WithTimeout(parent, wait+w.Timeout)
	defer cancel()
	started := time.Now()
	update, err := w.Source.EndpointAdmissions(ctx, w.Expected.NodeID(), w.Expected.ProcessEpoch(), since, wait)
	return endpointPull{update, err, time.Since(started), time.Now()}
}

// reconcileEndpoints pulls and applies the endpoint routes once, without waiting at the server.
func (w *RuntimeCarrierWorker) reconcileEndpoints(parent context.Context) {
	since, _ := w.nextEndpointPull()
	w.applyEndpointPull(parent, w.pullEndpoints(parent, since, 0))
}

// applyEndpointPull installs an endpoint answer and returns how long to pause before the next pull. A failed
// pull keeps the last good set and pauses endpoint pulls until the next runtime pull (once per Interval at
// most, so a failing server is not hammered); a pull that came back at once with nothing new waits out EndpointInterval (a
// server that does not hold, or is draining); anything else is pulled again right away.
func (w *RuntimeCarrierWorker) applyEndpointPull(parent context.Context, pull endpointPull) time.Duration {
	if waited := time.Since(pull.done); waited > time.Second {
		// The answer waited for this loop (a synchronous runtime pull or observation): new routes wait with it.
		slog.Warn("endpoint_pull_applied_late", "waited_ms", waited.Milliseconds())
	}
	if pull.err != nil {
		observeWorkerFailure(parent, w.Reporter, "endpoint_admission", pull.err)
		// A paused endpoint pull delays every new route until the next runtime pull: say so (tunnel create timing).
		slog.Warn("endpoint_pull_paused", "error", pull.err.Error(), "elapsed_ms", pull.elapsed.Milliseconds())
		w.endpointPause = true
		return w.EndpointInterval
	}
	news, failure := w.applyEndpoints(parent, pull.update)
	observeWorkerFailure(parent, w.Reporter, "endpoint_admission", failure)
	switch {
	case failure != nil:
		return w.EndpointInterval
	case !news && pull.elapsed < w.EndpointInterval:
		return w.EndpointInterval - pull.elapsed
	}
	return 0
}

// applyEndpoints merges an endpoint answer into the installed set. news reports a changed set or a tunnel
// assignment change this worker had not seen.
func (w *RuntimeCarrierWorker) applyEndpoints(parent context.Context, update control.EndpointUpdate) (news bool, failure error) {
	next, expiry := mergeEndpointUpdate(w.endpointSet, update)
	logEndpointRemovals(w.endpointSet, next, update)
	if update.Complete {
		w.endpointFullAt = time.Now()
	}
	w.endpointCursor = update.Cursor
	if update.TunnelsChangedAt != "" && update.TunnelsChangedAt != w.tunnelsChangedAt {
		w.tunnelsChangedAt = update.TunnelsChangedAt
		news = true
		slog.Info("endpoint_tunnels_changed", "changed_at", update.TunnelsChangedAt)
		if w.OnTunnelsChanged != nil {
			w.OnTunnelsChanged()
		}
	}
	w.endpointsPulled = true
	defer w.markPulled()
	changed := !endpointSetEqual(w.endpointSet, next)
	if !changed && expiry.Sub(w.endpointExpiry) < endpointRenewStep {
		return news, nil
	}
	w.endpointSet, w.endpointExpiry = next, expiry
	if err := w.install(); err != nil {
		return news || changed, err
	}
	if changed {
		w.withdraw()
		w.observe(parent)
	}
	return news || changed, nil
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

// detachReason names why a held route is detached: no longer expected, or expected with another admission.
func detachReason(expected bool) string {
	if expected {
		return "admission_changed"
	}
	return "not_expected"
}

// logEndpointRemovals logs each endpoint route of current that next drops: one the server answered
// as removed (a delta) or one missing from a full set.
func logEndpointRemovals(current, next []runtimewire.Admission, update control.EndpointUpdate) {
	kept := make(map[string]bool, len(next))
	for _, a := range next {
		kept[a.Binding.RouteID] = true
	}
	reason := "server_removed"
	if update.Complete {
		reason = "absent_from_full_set"
	}
	removed := removalLog{msg: "endpoint_route_removed"}
	for _, a := range current {
		if !kept[a.Binding.RouteID] {
			removed.add(a.Binding.RouteID, a.EndpointID, reason)
		}
	}
	removed.flush()
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
	detached := removalLog{msg: "runtime_route_detached"}
	defer detached.flush()
	allowed := map[string]datacarrier.ExpectedAdmission{}
	for _, a := range w.Expected.Snapshot() {
		if a.Admitted {
			allowed[a.RouteID] = a
		}
	}
	for _, r := range w.routes() {
		a, ok := allowed[r.RouteID]
		if !ok || a.Identity != r.Identity || a.AttachmentGeneration != r.AttachmentGeneration || a.RouteRevision != r.Revision {
			// A route a newer attachment of the same machine took over stays until that carrier
			// attaches it here (make-before-break at a worker handover; see datacarrier.supersededBy).
			if w.Expected.Superseded(r.Identity, r.RouteID, time.Now().UTC()) {
				continue
			}
			w.detachRoute(r, &detached, detachReason(ok))
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
	// drainEnd is set while the host's goaway lasts.
	var drainEnd <-chan time.Time
	defer func() {
		detached := removalLog{msg: "runtime_route_detached"}
		for _, a := range attached {
			w.detachAdmission(a, &detached, "carrier_closed")
		}
		detached.flush()
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
		detached := removalLog{msg: "runtime_route_detached"}
		for key, a := range attached {
			next, ok := desired[key]
			if ok {
				previous := a
				previous.ExpiresAt = next.ExpiresAt
				if runtimeAdmissionEqual(previous, next) {
					if err := w.registry(next).AttachAdmission(next, server); err == nil {
						attached[key] = next
						continue
					} else {
						logRuntimeAttachFailure(next, err)
					}
				}
			}
			if !ok || !runtimeAdmissionEqual(a, next) {
				w.detachAdmission(a, &detached, detachReason(ok))
				delete(attached, key)
				changed = true
			}
		}
		detached.flush()
		for key, a := range desired {
			if _, ok := attached[key]; !ok {
				if err := w.registry(a).AttachAdmission(a, server); err != nil {
					logRuntimeAttachFailure(a, err)
				} else {
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
		draining := server.Draining()
		if drainEnd != nil {
			draining = nil
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-server.Done():
			timer.Stop()
			return nil
		case <-changes:
		case <-timer.C:
		case <-draining:
			// The host is going away (T10). Its routes stay: the transports send new requests to
			// another live carrier of the route first and use this one only while it is the last,
			// so a worker handover loses no request before the new carrier attaches (cloudflared
			// unregisters a connection and serves on until its grace period ends). Open streams
			// finish. When the goaway ends the carrier is a normal one again.
			timer.Stop()
			drainEnd = time.After(max(time.Until(server.DrainUntil()), 0))
		case <-drainEnd:
			timer.Stop()
			drainEnd = nil
			if !server.EndDrain(time.Now()) && !server.DrainUntil().IsZero() {
				drainEnd = time.After(max(time.Until(server.DrainUntil()), 0))
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
	observeErr := w.Source.ObserveRuntimeCarriers(observeCtx, w.Expected.NodeID(), w.Expected.ProcessEpoch(), observations)
	if errors.Is(observeErr, control.ErrRuntimeCarrierStale) {
		observeErr = nil // the current carriers were stored; replaced ones need no report
	}
	return errors.Join(err, observeErr)
}

func browserTerminalFence(admission datacarrier.ExpectedAdmission) edgehttp.BrowserTerminalRouteFence {
	return edgehttp.BrowserTerminalRouteFence{Identity: admission.Identity, Revision: admission.RouteRevision, AttachmentGeneration: admission.AttachmentGeneration}
}

// runtimeAdmissionEqual compares two admissions apart from their expiry and admitted flag.
func runtimeAdmissionEqual(left, right datacarrier.ExpectedAdmission) bool {
	left.ExpiresAt = time.Time{}
	right.ExpiresAt = time.Time{}
	left.Admitted = false
	right.Admitted = false
	return left == right
}

// The carrier stays connected while its route is refused; this line names the route and the reason.
func logRuntimeAttachFailure(a datacarrier.ExpectedAdmission, err error) {
	if !attachFailureIsRefusal(err) {
		return
	}
	edgetelemetry.Log(context.Background(), sharedtelemetry.SeverityWarn, "runtime_route_attach_refused", sharedtelemetry.OutcomeRejected, "route_id", a.RouteID, "host_id", a.Identity.HostID, "session_id", a.Identity.SessionID, "attachment_generation", a.AttachmentGeneration, "error", err.Error())
}

// attachFailureIsRefusal is false for a registry that closed because this process is shutting down:
// no host was refused, and a replaced process logged 24 such lines at its last millisecond during the
// 2026-11-05 handover, which the deploy gate counts as refused route attaches.
func attachFailureIsRefusal(err error) bool {
	return !errors.Is(err, edgehttp.ErrDataCarrierPreviewRegistryClosed)
}
