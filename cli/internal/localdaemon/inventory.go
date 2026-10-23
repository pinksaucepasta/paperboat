package localdaemon

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"math/rand/v2"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

const (
	defaultRefreshInterval = 15 * time.Second
	defaultRequestTimeout  = 10 * time.Second
	minimumRefreshInterval = time.Second
	// One machine change bumps the inventory topic of every machine in its audience, so on a busy account
	// every daemon was woken every ~15 s and re-listed the account's machines (the largest server load).
	// Changes that arrive within the spacing are folded into one refresh. The spacing counts from the start of
	// the previous refresh, so a change shows locally within the spacing plus one refresh (the 30 s bound of
	// smoke check inventory-freshness; a refresh takes up to ~10 s on a slow control plane).
	defaultEventRefreshSpacing = 15 * time.Second
	// While the server's change stream is healthy the periodic refresh is only a safety net.
	defaultEventFallback = 5 * time.Minute
	// requestedRefreshSpacing is the least time between a refresh and one asked for by a local command
	// (pb wait, managed SSH after it starts): requests in between fold into one refresh at the spacing, so a
	// loop of commands costs the server at most one inventory per second.
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
	if outages < 32 && interval<<(outages-1) < outageMaximumRefresh {
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
	// EventRefreshSpacing is the least time between two refreshes woken by events (0: the default).
	// EventFallback is the refresh period while events are healthy (0: the default).
	EventRefreshSpacing time.Duration
	EventFallback       time.Duration
}

type Inventory struct {
	source            MachineSource
	store             *localapi.SnapshotStore
	refreshInterval   time.Duration
	requestTimeout    time.Duration
	clock             func() time.Time
	reconcileMachines func(context.Context, []api.UserMachine) error
	onMachines        func(context.Context, []api.UserMachine)
	onRefresh         func(error)
	events            *machineevents.Subscription
	eventSpacing      time.Duration
	eventFallback     time.Duration
	wake              chan struct{} // RequestRefresh; one pending request is enough
	mu                sync.Mutex
	completionMu      sync.RWMutex
	completion        localapi.CompletionSnapshot
}

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
		eventSpacing:      config.EventRefreshSpacing,
		eventFallback:     config.EventFallback,
		wake:              make(chan struct{}, 1),
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

func (i *Inventory) Snapshot(ctx context.Context) (localapi.Snapshot, error) {
	return i.store.Snapshot(ctx)
}

// Run refreshes once, then on the interval while there is no healthy change stream. With one, an inventory change
// refreshes at most once per eventSpacing, counted from the start of the previous refresh (changes in between fold
// into that refresh), and the interval only checks whether eventFallback has passed since the last refresh.
// A refresh that failed is retried on the outage backoff whatever the stream's health, so a local reconcile
// that failed while pb update restarted the services does not wait the event fallback.
func (i *Inventory) Run(ctx context.Context) error {
	var started time.Time        // of the latest refresh
	var pending <-chan time.Time // a change waiting for the spacing
	outages := 0                 // consecutive periodic checks that found the stream unhealthy or the refresh failed
	failed := false              // the latest refresh failed; retry it on the outage backoff, not the event fallback
	refresh := func() {
		pending = nil // this refresh shows every change so far
		started = time.Now()
		failed = i.Refresh(ctx) != nil
	}
	refresh()
	timer := time.NewTimer(i.refreshInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-i.wake:
			if pending != nil {
				continue
			}
			if wait := requestedRefreshSpacing - time.Since(started); wait > 0 {
				pending = time.After(wait)
				continue
			}
			refresh()
		case <-i.events.Changed():
			if pending != nil {
				continue
			}
			if wait := i.eventSpacing - time.Since(started); wait > 0 {
				pending = time.After(wait)
				continue
			}
			refresh()
		case <-pending:
			refresh()
		case <-timer.C:
			healthy := i.events.Healthy()
			if !healthy || failed || time.Since(started) >= i.eventFallback {
				refresh()
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

func (i *Inventory) Refresh(ctx context.Context) (refreshErr error) {
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
	var reconcileErr error
	if sourceErr == nil && i.reconcileMachines != nil {
		stage(func(stageCtx context.Context) { reconcileErr = i.reconcileMachines(stageCtx, machines) })
	}
	var completionItems []localapi.CompletionItem
	var completionErr error
	if sourceErr == nil {
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

	now := i.clock().UTC()
	if _, err := i.store.Update(now, func(current *localapi.Snapshot) (localapi.Snapshot, error) {
		desired := localapi.Snapshot{DaemonState: "ready", DaemonVersion: buildinfo.Version}
		if current != nil {
			desired.Machines = current.Machines
		}
		if sourceErr == nil {
			desired.Machines = preserveLocalObservations(mapMachines(machines), current)
			if reconcileErr != nil {
				desired.DaemonState = "degraded"
				// The local reconcile asks the server too (credentials, browser services and
				// certificates). When the server did not answer, the machine's local access is not what
				// is broken; saying so sent people to their local settings and made updates treat a slow
				// server as a broken installation (lanel-a, 2026-10-23).
				if controlPlaneUnreachable(reconcileErr) {
					desired.Health = controlPlaneUnavailableHealth(current, now)
				} else {
					desired.Health = []localapi.HealthItem{healthSince(current, now, localapi.HealthItem{
						Code:     "local_access_unavailable",
						Severity: "error",
						Title:    "Local machine access is unavailable",
						Recovery: "Check Paperboat local access settings and repair the installation",
					})}
				}
			}
		} else {
			desired.DaemonState = "degraded"
			desired.Health = controlPlaneUnavailableHealth(current, now)
			if inventoryAuthenticationRequired(sourceErr) {
				desired.DaemonState = "awaiting_enrollment"
				desired.Machines = nil
				desired.Health = []localapi.HealthItem{{Code: "authentication_required", Severity: "info", Title: "Paperboat is signed out on this computer (not added yet, or removed from the account)", Recovery: "Run `pb login` to sign in and add this computer", ETag: "authentication_required"}}
			}
		}
		return desired, nil
	}); err != nil {
		return errors.Join(refreshErr, err)
	}
	if inventoryAuthenticationRequired(sourceErr) {
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
	if sourceErr == nil && (completionErr == nil || len(completionItems) > 0 && !haveProjection) {
		snapshot := localapi.CompletionSnapshot{Schema: localapi.CompletionSchemaV1, ObservedAt: now, Items: append([]localapi.CompletionItem(nil), completionItems...)}
		if validationErr := snapshot.Validate(); validationErr == nil {
			i.completionMu.Lock()
			i.completion = snapshot
			i.completionMu.Unlock()
		} else {
			refreshErr = errors.Join(refreshErr, inventorySourceFailure("completion_projection", validationErr))
		}
	}
	if sourceErr == nil && i.onMachines != nil {
		i.onMachines(ctx, append([]api.UserMachine(nil), machines...))
	}
	return refreshErr
}

func controlPlaneUnavailableHealth(current *localapi.Snapshot, now time.Time) []localapi.HealthItem {
	return []localapi.HealthItem{healthSince(current, now, localapi.HealthItem{
		Code:     "control_plane_unavailable",
		Severity: "error",
		Title:    "Control plane is unavailable",
		Recovery: "Check network access and Paperboat authentication",
	})}
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
func controlPlaneUnreachable(err error) bool {
	var request *url.Error
	if errors.As(err, &request) {
		return true
	}
	var response *api.APIError
	return errors.As(err, &response) && (response.Status == 408 || response.Status == 429 || response.Status >= 500)
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
		} else if generation > 0 && !machine.SSHLocalReady {
			code := machine.SSHLocalCode
			if code == "" {
				code = "ssh_target_not_ready"
			}
			status.Health = []localapi.HealthItem{{Code: code, Severity: "warning", Title: "Managed SSH is not ready", Recovery: managedSSHDoctorRecovery, ETag: code}}
		} else if generation > 0 && machine.SSHAuthority.TargetGeneration != generation {
			status.Health = []localapi.HealthItem{{Code: "ssh_target_not_ready", Severity: "warning", Title: "SSH target is not ready", Recovery: "Ensure the machine's existing SSH service is listening on loopback", ETag: "ssh_target_not_ready"}}
		} else if generation > 0 && machine.SSHAuthority.HostKeyGeneration == 0 {
			// The machine never published SSH host keys. sshd may be missing, or running with host keys the
			// server refused as another machine's (a cloned image), so name both and the fix for each.
			status.Health = []localapi.HealthItem{{Code: "ssh_host_keys_unpublished", Severity: "warning", Title: "The machine's SSH host keys are not published", Recovery: sshHostKeysUnpublishedRecovery, ETag: "ssh_host_keys_unpublished"}}
		} else if generation > 0 && machine.SSHAuthority.HostKeyGeneration != generation {
			status.Health = []localapi.HealthItem{{Code: "ssh_host_key_changed", Severity: "warning", Title: "SSH host identity is not ready", Recovery: "Review and approve the machine's current SSH host keys", ETag: "ssh_host_key_changed"}}
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

const sshHostKeysUnpublishedRecovery = "On the machine, start an SSH server (sshd) if none runs. If sshd runs, its host keys are " +
	"identical to another machine's (cloned image): remove /etc/ssh/ssh_host_*, run ssh-keygen -A and restart sshd. " +
	"Paperboat publishes the keys within a minute"

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
