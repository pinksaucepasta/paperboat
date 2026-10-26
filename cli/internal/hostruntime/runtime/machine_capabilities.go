package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

const machineCapabilitiesSchemaV1 = "paperboat.machine-capabilities/v1"

type machineCapabilitySelection struct {
	Terminal      bool `json:"terminal"`
	ManagedSSH    bool `json:"managed_ssh"`
	FileReceive   bool `json:"file_receive"`
	PreviewTunnel bool `json:"preview_tunnel"`
}

type machineCapabilityPolicy struct {
	Schema         string                     `json:"schema"`
	Desired        machineCapabilitySelection `json:"desired"`
	DesiredVersion uint64                     `json:"desired_version"`
}

type machineCapabilitiesObservation struct {
	Schema     string                     `json:"schema"`
	Version    uint64                     `json:"version"`
	Applied    machineCapabilitySelection `json:"applied"`
	Status     string                     `json:"status"`
	ErrorCode  string                     `json:"error_code,omitempty"`
	ObservedAt time.Time                  `json:"observed_at"`
}

type machineCapabilityReconciler func(context.Context, machineCapabilitySelection, machineCapabilitySelection) error

// machineCapabilityController closes admission as soon as a newer desired
// policy arrives. It acknowledges that revision only after active-resource
// cleanup/reconciliation succeeds.
type machineCapabilityController struct {
	mu        sync.RWMutex
	desired   machineCapabilitySelection
	applied   machineCapabilitySelection
	version   uint64
	status    string
	errorCode string
	reconcile machineCapabilityReconciler
	// statePath keeps the last applied policy across restarts, so admission gates hold while Paperboat
	// is unreachable (F5-08, F21-11); generation fences it to this installation.
	statePath  string
	generation uint64
}

type persistedMachineCapabilities struct {
	InstallationGeneration uint64                     `json:"installation_generation"`
	Desired                machineCapabilitySelection `json:"desired"`
}

// Persist loads the last applied policy saved for this installation generation into the admission gate
// and saves each applied policy from then on. A file from another generation, or one that does not
// parse, is ignored. The loaded policy only opens gates; the first server answer is always applied
// and wins at once, including one that turns a capability off.
func (c *machineCapabilityController) Persist(path string, generation uint64) {
	if c == nil || path == "" || generation == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statePath, c.generation = path, generation
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 4<<10 {
		return
	}
	var saved persistedMachineCapabilities
	if json.Unmarshal(raw, &saved) != nil || saved.InstallationGeneration != generation {
		return
	}
	c.desired = saved.Desired
}

func (c *machineCapabilityController) saveLocked() error {
	if c.statePath == "" {
		return nil
	}
	raw, err := json.Marshal(persistedMachineCapabilities{InstallationGeneration: c.generation, Desired: c.applied})
	if err != nil {
		return err
	}
	return atomicfile.Write(c.statePath, raw, atomicfile.CurrentOwnerOptions(0o600))
}

func (c *machineCapabilityController) SetReconciler(reconcile machineCapabilityReconciler) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reconcile = reconcile
	c.mu.Unlock()
}

func (h *Host) reconcileMachineCapabilities(ctx context.Context, previous, next machineCapabilitySelection) error {
	if h == nil {
		return nil
	}
	var result error
	if h.reconcileCarrierSSH != nil {
		result = errors.Join(result, h.reconcileCarrierSSH(ctx, next.ManagedSSH))
	}
	if previous.Terminal && !next.Terminal {
		for _, snapshot := range h.sessions.List() {
			_, err := h.sessions.Close(ctx, snapshot.ID)
			result = errors.Join(result, err)
		}
		for _, snapshot := range h.executions.ActiveSnapshots() {
			execution, err := h.executions.Get(snapshot.OperationID)
			if err == nil {
				err = execution.Cancel(ctx)
			}
			result = errors.Join(result, err)
		}
	}
	if !next.ManagedSSH && h.dispatcher != nil {
		result = errors.Join(result, h.dispatcher.CloseSSHStreams())
		if h.managedSSHSelection != nil {
			result = errors.Join(result, h.managedSSHSelection.Disable(ctx))
		}
	}
	if previous.FileReceive && !next.FileReceive && h.transfers != nil {
		result = errors.Join(result, h.transfers.CancelActive(ctx))
	}
	return result
}

func newMachineCapabilityController(reconcile machineCapabilityReconciler) *machineCapabilityController {
	return &machineCapabilityController{status: "applied", reconcile: reconcile}
}

func (c *machineCapabilityController) Enabled(capability string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	desired := c.desired
	c.mu.RUnlock()
	switch capability {
	case "terminal.v1", "exec.v1":
		return desired.Terminal
	case "ssh.v1":
		return desired.ManagedSSH
	case "file-transfer.v1", "inbox.list.v1", "inbox.read.v1", "inbox.delete.v1":
		return desired.FileReceive
	case "preview.launch.v1", "private.access.v1":
		return desired.PreviewTunnel
	default:
		return true
	}
}

func (c *machineCapabilityController) Apply(ctx context.Context, policy machineCapabilityPolicy) error {
	if c == nil || policy.Schema != machineCapabilitiesSchemaV1 || policy.DesiredVersion < 1 {
		return errors.New("machine capability policy is invalid")
	}
	c.mu.Lock()
	if policy.DesiredVersion < c.version {
		c.mu.Unlock()
		return errors.New("machine capability policy is stale")
	}
	if policy.DesiredVersion == c.version && policy.Desired == c.desired && c.status == "applied" {
		c.mu.Unlock()
		return nil
	}
	previous := c.applied
	// Desired is the admission gate and changes before cleanup begins.
	c.desired, c.version, c.status, c.errorCode = policy.Desired, policy.DesiredVersion, "pending", ""
	c.mu.Unlock()
	var err error
	if c.reconcile != nil {
		err = c.reconcile(ctx, previous, policy.Desired)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.status, c.errorCode = "error", "capability_cleanup_failed"
		return err
	}
	c.applied, c.status, c.errorCode = policy.Desired, "applied", ""
	if err := c.saveLocked(); err != nil {
		slog.Warn("machine capabilities could not be saved for a restart while Paperboat is unreachable", "error", err)
	}
	return nil
}

func (c *machineCapabilityController) Observation(now time.Time) *machineCapabilitiesObservation {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.version == 0 {
		return nil
	}
	return &machineCapabilitiesObservation{Schema: machineCapabilitiesSchemaV1, Version: c.version, Applied: c.applied, Status: c.status, ErrorCode: c.errorCode, ObservedAt: now.UTC()}
}

func applyRuntimeMachineCapabilities(ctx context.Context, body []byte, controller *machineCapabilityController) (bool, error) {
	var response struct {
		Data struct {
			Policy     *machineCapabilityPolicy `json:"machine_capabilities"`
			Rejections []struct {
				Observation string `json:"observation"`
			} `json:"auxiliary_rejections"`
		} `json:"data"`
	}
	if controller == nil || json.Unmarshal(body, &response) != nil || response.Data.Policy == nil {
		return false, errors.New("runtime response has no machine capability policy")
	}
	if err := controller.Apply(ctx, *response.Data.Policy); err != nil {
		return false, err
	}
	for _, rejection := range response.Data.Rejections {
		if rejection.Observation == "machine_capabilities" {
			return false, nil
		}
	}
	return true, nil
}
