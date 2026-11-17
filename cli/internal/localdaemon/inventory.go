package localdaemon

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/httptransport"
)

const (
	// The periodic check while there is no healthy change stream, and the first step of its outage backoff: a
	// stream that flapped under server load reset the backoff and kept ~300 daemons re-listing every 15 s
	// (2026-11-16, Postgres at 200% CPU). Prior art: Tailscale's map poll backs off, never re-polls on a short timer.
	defaultRefreshInterval = time.Minute
	defaultRequestTimeout  = 10 * time.Second
	minimumRefreshInterval = time.Second
	// All ordinary wakes share a bounded debounce: topic moves from one change arrive together.
	defaultEventRefreshSpacing = 2 * time.Second
	// Reuse unrelated projections briefly, but invalidate immediately when their machine inputs change.
	inventoryProjectionFreshness = 15 * time.Second
	// While the server's change stream is healthy the periodic refresh is only a safety net.
	defaultEventFallback = 5 * time.Minute
	// The safety net re-lists machines (peer presence flips publish no event) but reuses the reconcile, completion and
	// warm-up of an unchanged list for this long: every part of them is event-driven, on-demand or has its own expiry
	// (PR-03: they were 7 of the 8 requests per 5 minutes of an idle Mac).
	healthyProjectionFreshness = 30 * time.Minute
	// requestedRefreshSpacing bounds retries while a local access step is pending. Ordinary wakes
	// use the debounce, so reconciliation cannot start a one-second refresh loop.
	requestedRefreshSpacing = time.Second
	// While it is unhealthy (usually the server itself is failing) the periodic refresh backs off from the
	// refresh interval to outageMaximumRefresh with jitter, so a fleet does not re-list every machine in
	// lockstep while the server recovers (22 machines made ~355 machine listings a minute in one outage).
	outageMaximumRefresh = 5 * time.Minute
)

// outageRefreshDelay is the wait before the next periodic check after outages consecutive checks found
// the change stream unhealthy: the interval itself while healthy, then doubling to outageMaximumRefresh,
// each with ±20% jitter.
func outageRefreshDelay(interval time.Duration, outages int) time.Duration {
	if outages == 0 {
		return interval
	}
	delay := outageMaximumRefresh
	// Compared by shifting the maximum down: interval<<(outages-1) overflowed to a negative delay and Int64N panicked.
	if outages < 32 && interval < outageMaximumRefresh>>(outages-1) {
		delay = interval << (outages - 1)
	}
	return delay - delay/5 + time.Duration(rand.Int64N(int64(delay/5)*2+1))
}

// FirstRefreshBudget bounds how long a newly started daemon with the default configuration takes to
// publish its first state: a refresh runs up to three stages (machine list, local reconcile, completion),
// each with its own defaultRequestTimeout. Until then it reports "starting".
const FirstRefreshBudget = 3 * defaultRequestTimeout

var ErrInvalidInventoryConfig = errors.New("invalid local daemon inventory configuration")

type MachineSource interface {
	ListUserMachines(context.Context) ([]api.UserMachine, error)
}

type CompletionInventorySource interface {
	ListCompletionItems(context.Context, []api.UserMachine) ([]localapi.CompletionItem, error)
}

type InventoryConfig struct {
	Source          MachineSource
	Store           *localapi.SnapshotStore
	RefreshInterval time.Duration
	RequestTimeout  time.Duration
	Clock           func() time.Time
	// ReconcileMachines must finish before a successful refresh can publish ready.
	ReconcileMachines func(context.Context, []api.UserMachine) error
	OnMachines        func(context.Context, []api.UserMachine)
	OnRefresh         func(error)
	// Events wakes a refresh when the server reports an inventory change; while its wait loop is
	// healthy the periodic refresh becomes a safety net. Nil refreshes on the interval only.
	Events *machineevents.Subscription
	// AccessEvents joins the inventory debounce for access changes; nil: none.
	AccessEvents *machineevents.Subscription
	// Nudge joins the debounce, or advances a pending local access step at requestedRefreshSpacing.
	Nudge <-chan struct{}
	// EventRefreshSpacing is the debounce after the first ordinary wake (0: the default).
	// EventFallback is the refresh period while events are healthy (0: the default).
	EventRefreshSpacing time.Duration
	EventFallback       time.Duration
	// CachePath keeps the last answered machine list for a restart while Paperboat is unreachable (inventory_cache.go);
	// empty: no cache.
	CachePath string
}

type Inventory struct {
	captive           atomic.Bool // the last control-plane failure was a captive portal
	source            MachineSource
	store             *localapi.SnapshotStore
	refreshInterval   time.Duration
	requestTimeout    time.Duration
	clock             func() time.Time
	reconcileMachines func(context.Context, []api.UserMachine) error
	onMachines        func(context.Context, []api.UserMachine)
	onRefresh         func(error)
	events            *machineevents.Subscription
	accessEvents      *machineevents.Subscription
	nudge             <-chan struct{}
	eventSpacing      time.Duration
	eventFallback     time.Duration
	wake              chan struct{} // RequestRefresh; one pending request is enough
	mu                sync.Mutex
	setupSince        time.Time // first local-access reconcile attempt (after start or pairing); bounds the grace
	reconciledOnce    bool      // a local-access reconcile has succeeded since start; guarded by store.Update
	completionMu      sync.RWMutex
	completion        localapi.CompletionSnapshot
	cachePath         string
	cached            []byte // the cache file's content as last read or written; guarded by mu after Run starts

	pendingLocalAccess atomic.Bool // last reconcile still had a local step
	projectionMachines []api.UserMachine
	reconcileAt        time.Time
	completionAt       time.Time
	onMachinesAt       time.Time
}

// localAccessStartupGrace is how long after start a failed local-access reconcile reads as starting, below the 60 s a
// supplied install waits for the new daemon.
const localAccessStartupGrace = 45 * time.Second

func NewInventory(config InventoryConfig) (*Inventory, error) {
	if config.Source == nil || config.Store == nil || config.RefreshInterval < 0 || config.RequestTimeout < 0 || config.EventRefreshSpacing < 0 || config.EventFallback < 0 {
		return nil, ErrInvalidInventoryConfig
	}
	if config.RefreshInterval == 0 {
		config.RefreshInterval = defaultRefreshInterval
	}
	if config.RefreshInterval < minimumRefreshInterval {
		return nil, ErrInvalidInventoryConfig
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.EventRefreshSpacing == 0 {
		config.EventRefreshSpacing = defaultEventRefreshSpacing
	}
	if config.EventFallback == 0 {
		config.EventFallback = defaultEventFallback
	}
	return &Inventory{
		source:            config.Source,
		store:             config.Store,
		refreshInterval:   config.RefreshInterval,
		requestTimeout:    config.RequestTimeout,
		clock:             config.Clock,
		reconcileMachines: config.ReconcileMachines,
		onMachines:        config.OnMachines,
		onRefresh:         config.OnRefresh,
		events:            config.Events,
		accessEvents:      config.AccessEvents,
		nudge:             config.Nudge,
		eventSpacing:      config.EventRefreshSpacing,
		eventFallback:     config.EventFallback,
		wake:              make(chan struct{}, 1),
		cachePath:         config.CachePath,
	}, nil
}

// RequestRefresh asks the running inventory for one more refresh soon, for a local change the server
// does not announce (managed SSH became ready after the first refresh). It never blocks.
func (i *Inventory) RequestRefresh() {
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

// SignedOut classifies a failed local operation from current authentication state.
// Refresh belongs to the inventory loop, never to the failure response path: a client retrying an unreachable
// peer re-listed every machine back to back (2026-11-16). Revocation reaches the loop by the change stream's
// refused wait, which wakes every consumer at once.
func (i *Inventory) SignedOut(ctx context.Context) bool {
	snapshot, err := i.store.Snapshot(context.WithoutCancel(ctx))
	return err == nil && snapshot.DaemonState == "awaiting_enrollment"
}

func (i *Inventory) Snapshot(ctx context.Context) (localapi.Snapshot, error) {
	return i.store.Snapshot(ctx)
}

// Run refreshes once, then coalesces topic moves and local wakes into one bounded debounce.
// Refused event waits refresh immediately; outages keep their existing backoff.
func (i *Inventory) Run(ctx context.Context) error {
	var started time.Time
	var pending <-chan time.Time
	var due time.Time
	forceReconcile := true
	requested := false
	outages := 0
	failed := false
	refreshWithin := func(reuse bool, freshness time.Duration) {
		pending = nil
		started = time.Now()
		failed = i.refresh(ctx, reuse && !requested, forceReconcile || !reuse, freshness) != nil
		forceReconcile = false
		requested = false
		// ApplyMachines can signal its own local-step transition. The completed reconcile already
		// observed it. Keep the wake when a pending step may have cleared during the refresh.
		if !i.pendingLocalAccess.Load() {
			select {
			case <-i.nudge:
			default:
			}
		}
	}
	refresh := func(reuse bool) { refreshWithin(reuse, inventoryProjectionFreshness) }
	i.restoreCachedMachines()
	refresh(false)
	timer := time.NewTimer(i.refreshInterval)
	defer timer.Stop()
	schedule := func(delay time.Duration, force bool) {
		forceReconcile = forceReconcile || force
		next := time.Now().Add(delay)
		if pending == nil || next.Before(due) {
			due = next
			pending = time.After(delay)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-i.wake:
			// Managed SSH finished starting (or the network changed): refresh its local
			// projection even if the server's machine inputs are identical.
			requested = true
			schedule(max(0, requestedRefreshSpacing-time.Since(started)), true)
		case <-i.nudge:
			delay := i.eventSpacing
			if i.pendingLocalAccess.Load() {
				delay = max(0, requestedRefreshSpacing-time.Since(started))
			}
			schedule(delay, true)
		case <-i.accessEvents.Changed():
			schedule(min(i.eventSpacing, defaultEventRefreshSpacing), true)
		case <-i.events.Changed():
			if !i.events.Healthy() {
				refresh(false)
			} else {
				// The stream is back: restart the outage backoff, so a listing that still fails right after the
				// recovery is retried on the interval, not minutes later (F21-01 W stayed unavailable 3m48s).
				if failed && outages > 0 {
					outages = 0
					timer.Reset(i.refreshInterval)
				}
				schedule(i.eventSpacing, false)
			}
		case <-pending:
			refresh(true)
		case <-timer.C:
			healthy := i.events.Healthy()
			switch {
			case !healthy || failed:
				refresh(false)
			case time.Since(started) >= i.eventFallback:
				refreshWithin(true, healthyProjectionFreshness)
			}
			if healthy && !failed {
				outages = 0
			} else {
				outages = min(outages+1, 32)
			}
			timer.Reset(outageRefreshDelay(i.refreshInterval, outages))
		}
	}
}

func (i *Inventory) Refresh(ctx context.Context) error {
	return i.refresh(ctx, false, true, inventoryProjectionFreshness)
}

func (i *Inventory) refresh(ctx context.Context, reuse, forceReconcile bool, freshness time.Duration) (refreshErr error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	defer func() {
		if i.onRefresh != nil {
			i.onRefresh(refreshErr)
		}
	}()

	// Each stage gets its own budget: on a slow control plane the machine list alone can use
	// most of one, and the local reconcile (which withdraws every machine route when it fails)
	// must not be cut off because the network was slow.
	stage := func(run func(context.Context)) {
		stageCtx, cancel := context.WithTimeout(ctx, i.requestTimeout)
		defer cancel()
		run(stageCtx)
	}
	var machines []api.UserMachine
	var sourceErr error
	stage(func(stageCtx context.Context) { machines, sourceErr = i.source.ListUserMachines(stageCtx) })
	sameMachines := reuse && reflect.DeepEqual(machines, i.projectionMachines)
	reuseReconcile := sameMachines && !forceReconcile && !i.pendingLocalAccess.Load() &&
		!i.reconcileAt.IsZero() && i.clock().Sub(i.reconcileAt) < freshness
	var reconcileErr error
	var pending LocalAccessPendingError
	if sourceErr == nil && i.reconcileMachines != nil && !reuseReconcile {
		i.reconcileAt = time.Time{}
		stage(func(stageCtx context.Context) { reconcileErr = i.reconcileMachines(stageCtx, machines) })
		// Native access is set up; one step (the browser certificate) is still being retried by the coordinator.
		if errors.As(reconcileErr, &pending) {
			reconcileErr = nil
		}
	}
	i.pendingLocalAccess.Store(pending.Step != "")
	reuseCompletion := sameMachines && !i.completionAt.IsZero() && i.clock().Sub(i.completionAt) < freshness
	reuseOnMachines := sameMachines && !i.onMachinesAt.IsZero() && i.clock().Sub(i.onMachinesAt) < freshness
	var completionItems []localapi.CompletionItem
	var completionErr error
	if sourceErr == nil && !reuseCompletion {
		i.completionAt = time.Time{}
		if source, ok := i.source.(CompletionInventorySource); ok {
			stage(func(stageCtx context.Context) {
				completionItems, completionErr = source.ListCompletionItems(stageCtx, machines)
			})
		}
	}
	for _, failure := range []error{sourceErr, reconcileErr, completionErr} {
		if failure == nil {
			continue
		}
		if refreshErr == nil {
			refreshErr = failure
		} else {
			refreshErr = errors.Join(refreshErr, failure)
		}
	}

	// The refresh handler answers authentication_required for api.ErrUnauthenticated; a signed-out credential or a
	// missing one is the same state, so the login that asked can tell it from a broken daemon.
	if (inventoryAuthenticationRequired(sourceErr) || inventoryAuthenticationRequired(reconcileErr)) && !errors.Is(refreshErr, api.ErrUnauthenticated) {
		refreshErr = errors.Join(refreshErr, api.ErrUnauthenticated)
	}

	portal := i.captivePortalBlocks(ctx, sourceErr, reconcileErr)
	i.captive.Store(portal)
	now := i.clock().UTC()
	if sourceErr == nil && reconcileErr == nil && !reuseReconcile {
		i.reconcileAt = now
	}
	var answered []localapi.MachineStatus
	if _, err := i.store.Update(now, func(current *localapi.Snapshot) (localapi.Snapshot, error) {
		desired := localapi.Snapshot{DaemonState: "ready", DaemonVersion: buildinfo.Version}
		if current != nil {
			desired.Machines, desired.SSHDomain = current.Machines, current.SSHDomain
		}
		if sourceErr == nil {
			desired.Machines = preserveLocalObservations(mapMachines(machines), current)
			answered = desired.Machines
			// The first reconcile runs once the machine list answers: right after start, or after `pb pair` when the
			// daemon was waiting for enrollment. The grace counts from then, so a slow first setup is starting either way.
			if i.setupSince.IsZero() {
				i.setupSince = now
			}
			if reconcileErr == nil {
				i.reconciledOnce = true
			}
			if pending.Step != "" {
				desired.Health = []localapi.HealthItem{healthSince(current, now, localapi.HealthItem{
					Code:     "browser_urls_unavailable",
					Severity: "warning",
					Title:    pendingStepTitle(pending.Step, pending.Detail),
					Recovery: pendingStepRecovery(pending.Step),
				})}
			}
			if reconcileErr != nil {
				desired.DaemonState = "degraded"
				// A daemon that just started has not finished its first local-access setup (the machine guard and
				// loopback addresses can lag its start): it is starting, not broken, for a bounded grace. A
				// supplied install's validator waits for starting and fails on degraded (F14-03).
				if !i.reconciledOnce && now.Sub(i.setupSince) < localAccessStartupGrace {
					desired.DaemonState = "starting"
				}
				// The local reconcile asks the server too (credentials, browser services and
				// certificates). When the server did not answer, the machine's local access is not what
				// is broken; saying so sent people to their local settings and made updates treat a slow
				// server as a broken installation (lanel-a, 2026-10-23).
				switch {
				case inventoryAuthenticationRequired(reconcileErr):
					// A server 401 or a signed-out credential during the reconcile is the signed-out state, not a
					// broken installation: "repair the installation" cannot fix it (pbvm2, 2026-11-09).
					signedOutSnapshot(&desired)
				case controlPlaneUnreachable(reconcileErr, i.apiHost()):
					desired.Health = controlPlaneUnavailableHealth(current, now, reconcileErr, portal)
				default:
					item := healthSince(current, now, localapi.HealthItem{
						Code:     "local_access_unavailable",
						Severity: "error",
						Title:    localAccessTitle("Local machine access is unavailable", reconcileErr),
						Recovery: "Check Paperboat local access settings and repair the installation",
					})
					// One journal line per change of the cause, not one per retry.
					if current == nil || len(current.Health) != 1 || current.Health[0].Title != item.Title {
						diagnosticlog.TryWarn("local machine access is unavailable", "cause", LocalAccessCause(reconcileErr))
					}
					desired.Health = []localapi.HealthItem{item}
				}
			}
		} else {
			desired.DaemonState = "degraded"
			desired.Health = controlPlaneUnavailableHealth(current, now, sourceErr, portal)
			if !controlPlaneUnreachable(sourceErr, i.apiHost()) {
				// A local step of the listing failed (configuration, credentials, alias validation), not the server.
				desired.Health = []localapi.HealthItem{healthSince(current, now, localapi.HealthItem{
					Code:     "machine_list_unavailable",
					Severity: "error",
					Title:    "Paperboat could not read your machine list on this computer",
					Recovery: "Run pb doctor, then repair the installation if it reports a problem",
				})}
			}
			if inventoryAuthenticationRequired(sourceErr) {
				signedOutSnapshot(&desired)
			}
		}
		return desired, nil
	}); err != nil {
		return errors.Join(refreshErr, err)
	}
	if sourceErr == nil {
		i.saveCachedMachines(answered)
	}
	if inventoryAuthenticationRequired(sourceErr) || inventoryAuthenticationRequired(reconcileErr) {
		i.forgetCachedMachines()
		i.completionMu.Lock()
		i.completion = localapi.CompletionSnapshot{}
		i.completionMu.Unlock()
	}
	// A partial list (machine names, without previews or sessions the server could not list) is
	// published only while there is no good projection yet, so a fresh daemon still completes machine
	// names; an existing projection is kept through the outage.
	i.completionMu.RLock()
	haveProjection := i.completion.Validate() == nil
	i.completionMu.RUnlock()
	if sourceErr == nil && !reuseCompletion && (completionErr == nil || len(completionItems) > 0 && !haveProjection) {
		snapshot := localapi.CompletionSnapshot{Schema: localapi.CompletionSchemaV1, ObservedAt: now, Items: append([]localapi.CompletionItem(nil), completionItems...)}
		if validationErr := snapshot.Validate(); validationErr == nil {
			i.completionMu.Lock()
			i.completion = snapshot
			if completionErr == nil {
				i.completionAt = now
			}
			i.completionMu.Unlock()
		} else {
			refreshErr = errors.Join(refreshErr, inventorySourceFailure("completion_projection", validationErr))
		}
	}
	if sourceErr == nil && i.onMachines != nil && !reuseOnMachines {
		i.onMachines(ctx, append([]api.UserMachine(nil), machines...))
		i.onMachinesAt = now
	}
	if sourceErr == nil {
		i.projectionMachines = append([]api.UserMachine(nil), machines...)
	}
	if inventoryAuthenticationRequired(sourceErr) || inventoryAuthenticationRequired(reconcileErr) {
		i.completionAt, i.onMachinesAt, i.reconcileAt = time.Time{}, time.Time{}, time.Time{}
		i.projectionMachines = nil
	}
	return refreshErr
}

// apiHost is the configured Paperboat API's host, from the machine source.
func (i *Inventory) apiHost() string {
	if source, ok := i.source.(interface{ APIHost() string }); ok {
		return source.APIHost()
	}
	return ""
}

// captivePortalBlocks probes for a captive portal only when the control plane just failed to answer.
func (i *Inventory) captivePortalBlocks(ctx context.Context, causes ...error) bool {
	source, ok := i.source.(interface{ CaptivePortal(context.Context) bool })
	if !ok {
		return false
	}
	for _, cause := range causes {
		if cause != nil && controlPlaneUnreachable(cause, i.apiHost()) {
			probeCtx, cancel := context.WithTimeout(ctx, i.requestTimeout)
			defer cancel()
			return source.CaptivePortal(probeCtx)
		}
	}
	return false
}

func controlPlaneUnavailableHealth(current *localapi.Snapshot, now time.Time, cause error, portal bool) []localapi.HealthItem {
	item := localapi.HealthItem{
		Code:     "control_plane_unavailable",
		Severity: "error",
		Title:    "Control plane is unavailable",
		Recovery: "Check network access and Paperboat authentication",
	}
	if portal { // same item (the updater and local access gates key on its code), named cause
		item.Title, item.Recovery = api.CaptivePortalMessage, "Sign in to the network in a browser; Paperboat reconnects on its own once the network is open."
		return []localapi.HealthItem{healthSince(current, now, item)}
	}
	var proxy *httptransport.ProxyError
	if errors.As(cause, &proxy) && proxy.Failure == httptransport.ProxyUnreachable {
		item.Title = "Control plane is unavailable: the configured proxy did not accept the connection"
		item.Recovery = "Start the proxy, or remove PAPERBOAT_HTTPS_PROXY (service environment) and the system proxy setting, then restart the daemon"
	}
	return []localapi.HealthItem{healthSince(current, now, item)}
}

// healthSince completes a single health item, keeping when it broke if the current snapshot already
// reports the same one.
func healthSince(current *localapi.Snapshot, now time.Time, item localapi.HealthItem) localapi.HealthItem {
	brokenSince := now
	if current != nil && len(current.Health) == 1 && current.Health[0].Code == item.Code && current.Health[0].BrokenSince != nil {
		brokenSince = *current.Health[0].BrokenSince
	}
	item.BrokenSince, item.ETag = &brokenSince, item.Code
	return item
}

// controlPlaneUnreachable reports a failure of a request to the Paperboat server itself: the HTTP
// client could not complete it (*url.Error: connection, TLS or timeout), or the server answered
// 408/429/5xx. A local failure (the machine guard socket, or the reconcile's own deadline with no
// request in the chain) is not the control plane. The reconcile's only net/http requests go to the
// server; the machine guard speaks its own protocol over a unix socket (*net.OpError). If a local
// dependency ever moves to HTTP, this must look at the request's host instead of the error type.
// controlPlaneUnreachable reports whether err is the Paperboat API not answering: a request to apiHost that
// failed in transport, or an API answer of 408, 429 or 5xx. A failed request to any other host (a peer, a local
// service) is not the control plane: labeling it so made a healthy update roll back (hp, 2026-10-27).
func controlPlaneUnreachable(err error, apiHost string) bool {
	var request *url.Error
	if errors.As(err, &request) {
		target, parseErr := url.Parse(request.URL)
		return parseErr == nil && apiHost != "" && strings.EqualFold(target.Host, apiHost)
	}
	var response *api.APIError
	if errors.As(err, &response) {
		return response.Status == 408 || response.Status == 429 || response.Status >= 500
	}
	// A Wi-Fi sign-in page answers 200 HTML after the client followed its redirect: the server did not answer.
	var invalid *api.ResponseDecodeError
	return errors.As(err, &invalid)
}

func (i *Inventory) Completions(context.Context) (localapi.CompletionSnapshot, error) {
	if i == nil {
		return localapi.CompletionSnapshot{}, localapi.ErrInvalidResponse
	}
	i.completionMu.RLock()
	result := i.completion
	result.Items = append([]localapi.CompletionItem(nil), result.Items...)
	i.completionMu.RUnlock()
	if result.Validate() != nil {
		return localapi.CompletionSnapshot{}, localapi.ErrInvalidResponse
	}
	return result, nil
}

func preserveLocalObservations(machines []localapi.MachineStatus, current *localapi.Snapshot) []localapi.MachineStatus {
	if current == nil {
		return machines
	}
	observations := make(map[string]localapi.MachineStatus, len(current.Machines))
	for _, machine := range current.Machines {
		observations[machine.ID] = machine
	}
	for index := range machines {
		if observed, ok := observations[machines[index].ID]; ok {
			machines[index].ActiveConsumers = observed.ActiveConsumers
			machines[index].SelectedPath = observed.SelectedPath
			machines[index].StandbyPath = observed.StandbyPath
			machines[index].RelayRegion = observed.RelayRegion
			machines[index].NATMappingIPv4 = observed.NATMappingIPv4
			machines[index].NATMappingIPv6 = observed.NATMappingIPv6
			machines[index].CaptivePortal = observed.CaptivePortal
			machines[index].PMTU = observed.PMTU
			machines[index].RouterMapping = observed.RouterMapping
			machines[index].RouterProtocol = observed.RouterProtocol
			machines[index].MappingLifetime = observed.MappingLifetime
		}
	}
	return machines
}

func mapMachines(machines []api.UserMachine) []localapi.MachineStatus {
	result := make([]localapi.MachineStatus, 0, len(machines))
	for _, machine := range machines {
		generation := uint64(0)
		if machine.InstallationGeneration > 0 {
			generation = uint64(machine.InstallationGeneration)
		}
		status := localapi.MachineStatus{
			ID:                machine.ID,
			EnvironmentID:     machine.EnvironmentID,
			WorkspaceRoot:     machine.WorkspaceRoot,
			Alias:             machine.Alias,
			Ownership:         machine.Ownership,
			OwnerAccount:      machine.OwnerAccount,
			Tags:              machine.Tags,
			Platform:          machine.Platform,
			Eligible:          generation > 0 && machine.State != "revoked" && machine.State != "deleted",
			RuntimeState:      runtimeState(machine, generation),
			Generation:        generation,
			LastObservedAt:    machine.RuntimeDiagnostics.ObservedAt,
			SelectedPath:      "none",
			StandbyPath:       "none",
			TransferReadiness: capabilityReadiness(machine.Online, machine.Capabilities.FileReceive),
			PreviewReadiness:  capabilityReadiness(machine.Online, machine.Capabilities.PreviewLaunch),
			SSHReadiness:      sshReadiness(machine, generation),
			SSHUser:           machine.SSHUser,
			SSHPort:           machine.SSHPort,
			SSHHostKeys:       machine.SSHHostKeys,
			NATMappingIPv4:    "unknown",
			NATMappingIPv6:    "unknown",
			CaptivePortal:     "unknown",
			PMTU:              "unknown",
			RouterMapping:     "unknown",
			RouterProtocol:    "unknown",
			MappingLifetime:   "unknown",
			UpdateHealth:      updateHealth(machine),
		}
		if machine.State == "revoked" || machine.State == "deleted" {
			status.Health = []localapi.HealthItem{{Code: "machine_unavailable", Severity: "error", Title: "Machine is unavailable", Recovery: "Set up the machine again", ETag: "machine_unavailable"}}
		} else if !machine.Online && generation > 0 {
			status.Health = []localapi.HealthItem{{Code: "runtime_offline", Severity: "warning", Title: "Machine runtime is offline", Recovery: "Check the Paperboat service on the machine", ETag: "runtime_offline"}}
		} else if generation > 0 && !machine.SSHLocalReady && machine.SSHLocalCode != "" {
			// Without a code this computer's managed SSH is still starting: nothing to report or refuse yet.
			code := machine.SSHLocalCode
			status.Health = []localapi.HealthItem{{Code: code, Severity: "warning", Title: "Managed SSH is not ready", Recovery: managedSSHDoctorRecovery, ETag: code}}
			if code == managedSSHServerRefusedCode {
				status.Health[0].Title = "Managed SSH is not ready: the server refused this computer's managed SSH request"
				status.Health[0].Recovery = "Run pb ssh doctor <machine>; if it repeats, run pb logout and pb login, then pb bugreport --upload"
			}
			if code == managedSSHUnsafeDirectoryCode {
				status.Health[0].Title = "This computer cannot use managed SSH: its .ssh folder or config is owned by another account or writable by other users"
				status.Health[0].Recovery = "Run pb ssh doctor <machine>: it names the folder or file, the account at fault and the icacls commands that fix it"
			}
		} else if generation > 0 && machine.SSHAuthority.TargetGeneration != generation {
			status.Health = []localapi.HealthItem{{Code: "ssh_target_not_ready", Severity: "warning", Title: "SSH target is not ready", Recovery: "Ensure the machine's existing SSH service is listening on loopback", ETag: "ssh_target_not_ready"}}
			if machine.Platform == "windows" {
				// Windows has no SSH service of its own to point at: Paperboat installs OpenSSH there and
				// pairing finishes without it when the machine cannot supply the package.
				status.Health[0].Title, status.Health[0].Recovery = "Managed SSH is not ready", managedSSHWindowsTargetRecovery
			}
		} else if generation > 0 && machine.SSHAuthority.HostKeyGeneration == 0 && machine.SSHAuthority.HostKeyInUse {
			status.Health = []localapi.HealthItem{hostKeyInUseHealth(machine.SSHAuthority.HostKeyInUseBy)}
		} else if generation > 0 && machine.SSHAuthority.HostKeyGeneration == 0 {
			// Not refused (the server lists a refusal): the host has not published its keys yet, or sshd is missing.
			status.Health = []localapi.HealthItem{{Code: "ssh_host_keys_unpublished", Severity: "info", Title: "Publishing the machine's SSH host keys", Recovery: sshHostKeysUnpublishedRecovery, ETag: "ssh_host_keys_unpublished"}}
			if machine.Platform == "windows" {
				// There is no sshd to start on Windows: Paperboat installs its own OpenSSH, and repair is the one command.
				status.Health[0].Recovery = sshHostKeysUnpublishedWindowsRecovery
			}
		} else if generation > 0 && machine.SSHAuthority.HostKeyGeneration != generation {
			status.Health = []localapi.HealthItem{{Code: "ssh_host_key_changed", Severity: "warning", Title: "SSH host identity is not ready", Recovery: "Review and approve the machine's current SSH host keys", ETag: "ssh_host_key_changed"}}
		}
		if len(status.Health) == 0 {
			// Every degraded readiness names why: a machine that just came online has services still starting
			// (managed SSH before it reports, transfer or preview before they are observed).
			if item, ok := startingServicesHealth(status); ok {
				status.Health = []localapi.HealthItem{item}
			}
		}
		result = append(result, status)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Alias == result[right].Alias {
			return result[left].ID < result[right].ID
		}
		return result[left].Alias < result[right].Alias
	})
	return result
}

const managedSSHWindowsTargetRecovery = "If this stays, install \"App Installer\" (winget) from the Microsoft Store " +
	"on the machine, then run pb doctor --repair there"

// The Windows form of sshHostKeysUnpublishedRecovery: the same wait, then the Windows step.
const sshHostKeysUnpublishedWindowsRecovery = "A newly set up machine publishes them within a minute or two. " + managedSSHWindowsTargetRecovery

// A machine shows this from pairing until its host service has installed and published the keys (about a minute
// on a fresh container, dadape ltc-d7 2026-10-27).
const sshHostKeysUnpublishedRecovery = "A newly set up machine publishes them within a minute or two. If this stays, " +
	"start an SSH server (sshd) on the machine"

// hostKeyInUseHealth is the server's refusal of the machine's host keys as another live machine's (a cloned VM or
// image). other is empty when the caller may not see that machine.
func hostKeyInUseHealth(other string) localapi.HealthItem {
	title := "The machine's SSH host keys are identical to another machine's"
	if other != "" {
		title = "The machine's SSH host keys are identical to " + other + "'s"
	}
	return localapi.HealthItem{Code: "ssh_host_key_in_use", Severity: "warning", Title: title, Recovery: "This machine was " +
		"cloned with its SSH host keys. On it, remove /etc/ssh/ssh_host_*, run ssh-keygen -A and restart sshd; " +
		"Paperboat publishes the new keys within a minute", ETag: "ssh_host_key_in_use"}
}

func updateHealth(machine api.UserMachine) string {
	if machine.Availability.Schema != "paperboat.availability-policy/v1" || machine.Availability.ObservedAt == nil {
		return "unknown"
	}
	switch machine.Availability.UpdateHealth {
	case "healthy", "recovery_required":
		return machine.Availability.UpdateHealth
	default:
		return "unknown"
	}
}

// startingServicesHealth is the item for readiness that is degraded while nothing else explains it.
func startingServicesHealth(status localapi.MachineStatus) (localapi.HealthItem, bool) {
	var starting []string
	for _, service := range []struct{ name, readiness string }{{"SSH", status.SSHReadiness}, {"file transfer", status.TransferReadiness}, {"previews", status.PreviewReadiness}} {
		if service.readiness == "degraded" {
			starting = append(starting, service.name)
		}
	}
	if len(starting) == 0 {
		return localapi.HealthItem{}, false
	}
	return localapi.HealthItem{Code: "machine_services_starting", Severity: "info", Title: "Still starting on this machine: " + strings.Join(starting, ", "), Recovery: "No action is needed; this clears within a minute of the machine coming online. If it does not, run pb doctor.", ETag: "machine_services_starting"}, true
}

func sshReadiness(machine api.UserMachine, generation uint64) string {
	if generation == 0 || machine.State == "revoked" || machine.State == "deleted" || machine.SSHAuthority.TargetGeneration == 0 {
		return "unavailable"
	}
	if !machine.Online || !machine.SSHLocalReady || machine.SSHAuthority.TargetGeneration != generation || machine.SSHAuthority.HostKeyGeneration != generation {
		return "degraded"
	}
	return "ready"
}

func runtimeState(machine api.UserMachine, generation uint64) string {
	if machine.State == "revoked" || machine.State == "deleted" {
		return "failed"
	}
	if machine.Online {
		return "ready"
	}
	if generation == 0 {
		return "stopped"
	}
	return "offline"
}

func capabilityReadiness(online bool, capability api.MachineCapability) string {
	if !capability.Configured {
		return "unavailable"
	}
	if online && capability.Observed {
		return "ready"
	}
	return "degraded"
}

// LocalAccessPendingError reports a reconciliation that set up native access while one local access step
// (Step, for example browser_certificate) failed and is being retried: the daemon stays ready and pb doctor
// names the step.
type LocalAccessPendingError struct{ Step, Detail string }

func (e LocalAccessPendingError) Error() string {
	return "local access step " + e.Step + " is retrying"
}

// signedOutSnapshot is the state of a computer the server no longer accepts: no machines, and the way back in.
func signedOutSnapshot(desired *localapi.Snapshot) {
	desired.DaemonState = "awaiting_enrollment"
	desired.Machines = nil
	desired.Health = []localapi.HealthItem{{Code: "authentication_required", Severity: "info", Title: "Paperboat is signed out on this computer (not added yet, or removed from the account)", Recovery: "Run `pb login` to sign in and add this computer", ETag: "authentication_required"}}
}

// LocalAccessCause names the failing local access step (when the error carries one) and the error's class from
// the finite cause vocabulary, never its text: health titles and the journal can show it without secrets.
func LocalAccessCause(err error) string {
	fault := errorreport.ProjectFault(context.Background(), "paperboatd", "local_access", "reconciliation", "local_access_failed", err)
	class := fault.Cause
	if fault.HTTPStatus != 0 {
		class += ", HTTP " + strconv.Itoa(fault.HTTPStatus)
	}
	if fault.Cause == "internal" {
		class += ", " + fault.ErrorType // an unclassified failure still says which kind of error it was
	}
	var step interface{ LocalAccessStep() string }
	if errors.As(err, &step) && step.LocalAccessStep() != "" {
		return "the " + strings.ReplaceAll(step.LocalAccessStep(), "_", " ") + " step failed (" + class + ")"
	}
	return "a local access step failed (" + class + ")"
}

func localAccessTitle(title string, err error) string { return title + ": " + LocalAccessCause(err) }

// pendingStepRecovery says what to do about a retrying local access step.
func pendingStepRecovery(step string) string {
	if step == "browser_gateway_port" {
		return "Stop that program, and Paperboat turns browser URLs back on by itself."
	}
	return "The rest of local access works; Paperboat retries the step by itself"
}

// pendingStepTitle says what a retrying local access step leaves unavailable.
func pendingStepTitle(step, detail string) string {
	what := "Some machine names are unavailable"
	switch step {
	case "browser_gateway_port":
		title := "Browser URLs are off because another program is using port 80/443"
		if detail != "" {
			title += " (" + detail + ")"
		}
		return title
	case "browser_certificate":
		what = "Browser URLs are unavailable"
	case "browser_port_listener":
		what = "Some https://<machine>:<port> URLs are unavailable (https://<port>.<machine> works)"
	}
	return what + ": the " + strings.ReplaceAll(step, "_", " ") + " step failed and is retrying"
}
