package localdaemon

import (
	"context"
	"errors"
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
)

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
	mu                sync.Mutex
	completionMu      sync.RWMutex
	completion        localapi.CompletionSnapshot
}

func NewInventory(config InventoryConfig) (*Inventory, error) {
	if config.Source == nil || config.Store == nil || config.RefreshInterval < 0 || config.RequestTimeout < 0 {
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
	return &Inventory{
		source:            config.Source,
		store:             config.Store,
		refreshInterval:   config.RefreshInterval,
		requestTimeout:    config.RequestTimeout,
		clock:             config.Clock,
		reconcileMachines: config.ReconcileMachines,
		onMachines:        config.OnMachines,
		onRefresh:         config.OnRefresh,
	}, nil
}

func (i *Inventory) Run(ctx context.Context) error {
	_ = i.Refresh(ctx)
	return i.runTicker(ctx)
}

func (i *Inventory) runTicker(ctx context.Context) error {
	ticker := time.NewTicker(i.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = i.Refresh(ctx)
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

	requestCtx, cancel := context.WithTimeout(ctx, i.requestTimeout)
	machines, sourceErr := i.source.ListUserMachines(requestCtx)
	var reconcileErr error
	if sourceErr == nil && i.reconcileMachines != nil {
		reconcileErr = i.reconcileMachines(requestCtx, machines)
	}
	var completionItems []localapi.CompletionItem
	var completionErr error
	if sourceErr == nil {
		if source, ok := i.source.(CompletionInventorySource); ok {
			completionItems, completionErr = source.ListCompletionItems(requestCtx, machines)
		}
	}
	cancel()
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
				brokenSince := now
				if current != nil && len(current.Health) == 1 && current.Health[0].Code == "local_access_unavailable" && current.Health[0].BrokenSince != nil {
					brokenSince = *current.Health[0].BrokenSince
				}
				desired.DaemonState = "degraded"
				desired.Health = []localapi.HealthItem{{
					Code:        "local_access_unavailable",
					Severity:    "error",
					Title:       "Local machine access is unavailable",
					BrokenSince: &brokenSince,
					Recovery:    "Check Paperboat local access settings and repair the installation",
					ETag:        "local_access_unavailable",
				}}
			}
		} else {
			brokenSince := now
			if current != nil && len(current.Health) == 1 && current.Health[0].Code == "control_plane_unavailable" && current.Health[0].BrokenSince != nil {
				brokenSince = *current.Health[0].BrokenSince
			}
			desired.DaemonState = "degraded"
			desired.Health = []localapi.HealthItem{{
				Code:        "control_plane_unavailable",
				Severity:    "error",
				Title:       "Control plane is unavailable",
				BrokenSince: &brokenSince,
				Recovery:    "Check network access and Paperboat authentication",
				ETag:        "control_plane_unavailable",
			}}
			if inventoryAuthenticationRequired(sourceErr) {
				desired.DaemonState = "awaiting_enrollment"
				desired.Machines = nil
				desired.Health = []localapi.HealthItem{{Code: "authentication_required", Severity: "info", Title: "Paperboat is installed and waiting for sign-in", Recovery: "Enroll this machine from the Paperboat dashboard", ETag: "authentication_required"}}
			}
		}
		return desired, nil
	}); err != nil {
		return errors.Join(refreshErr, err)
	}
	if sourceErr == nil && completionErr == nil {
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
