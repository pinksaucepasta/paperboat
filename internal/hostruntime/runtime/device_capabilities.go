package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const deviceCapabilitiesSchemaV1 = "paperboat.device-capabilities/v1"

type deviceCapabilitySelection struct {
	Terminal      bool `json:"terminal"`
	ManagedSSH    bool `json:"managed_ssh"`
	FileReceive   bool `json:"file_receive"`
	PreviewTunnel bool `json:"preview_tunnel"`
	PeerRelay     bool `json:"peer_relay"`
}

type deviceCapabilityPolicy struct {
	Schema         string                    `json:"schema"`
	Desired        deviceCapabilitySelection `json:"desired"`
	DesiredVersion uint64                    `json:"desired_version"`
}

type deviceCapabilitiesObservation struct {
	Schema     string                    `json:"schema"`
	Version    uint64                    `json:"version"`
	Applied    deviceCapabilitySelection `json:"applied"`
	Status     string                    `json:"status"`
	ErrorCode  string                    `json:"error_code,omitempty"`
	ObservedAt time.Time                 `json:"observed_at"`
}

type deviceCapabilityReconciler func(context.Context, deviceCapabilitySelection, deviceCapabilitySelection) error

// deviceCapabilityController closes admission as soon as a newer desired
// policy arrives. It acknowledges that revision only after active-resource
// cleanup/reconciliation succeeds.
type deviceCapabilityController struct {
	mu        sync.RWMutex
	desired   deviceCapabilitySelection
	applied   deviceCapabilitySelection
	version   uint64
	status    string
	errorCode string
	reconcile deviceCapabilityReconciler
}

func (c *deviceCapabilityController) SetReconciler(reconcile deviceCapabilityReconciler) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reconcile = reconcile
	c.mu.Unlock()
}

func (h *Host) reconcileDeviceCapabilities(ctx context.Context, previous, next deviceCapabilitySelection) error {
	if h == nil {
		return nil
	}
	var result error
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
	if previous.ManagedSSH && !next.ManagedSSH && h.dispatcher != nil {
		result = errors.Join(result, h.dispatcher.CloseSSHStreams())
	}
	if previous.FileReceive && !next.FileReceive && h.transfers != nil {
		result = errors.Join(result, h.transfers.CancelActive(ctx))
	}
	if previous.PeerRelay != next.PeerRelay && h.deviceRelay != nil {
		result = errors.Join(result, h.deviceRelay.ReconcilePeerRelay(ctx, next.PeerRelay))
	}
	return result
}

func newDeviceCapabilityController(reconcile deviceCapabilityReconciler) *deviceCapabilityController {
	return &deviceCapabilityController{status: "applied", reconcile: reconcile}
}

func (c *deviceCapabilityController) Enabled(capability string) bool {
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
	case "file-transfer.v1":
		return desired.FileReceive
	case "preview.launch.v1", "private.access.v1":
		return desired.PreviewTunnel
	case "peer-relay.v1":
		return desired.PeerRelay
	default:
		return true
	}
}

func (c *deviceCapabilityController) Apply(ctx context.Context, policy deviceCapabilityPolicy) error {
	if c == nil || policy.Schema != deviceCapabilitiesSchemaV1 || policy.DesiredVersion < 1 {
		return errors.New("device capability policy is invalid")
	}
	c.mu.Lock()
	if policy.DesiredVersion < c.version {
		c.mu.Unlock()
		return errors.New("device capability policy is stale")
	}
	if policy.DesiredVersion == c.version && policy.Desired == c.desired {
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
	return nil
}

func (c *deviceCapabilityController) Observation(now time.Time) *deviceCapabilitiesObservation {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.version == 0 {
		return nil
	}
	return &deviceCapabilitiesObservation{Schema: deviceCapabilitiesSchemaV1, Version: c.version, Applied: c.applied, Status: c.status, ErrorCode: c.errorCode, ObservedAt: now.UTC()}
}

func applyRuntimeDeviceCapabilities(ctx context.Context, body []byte, controller *deviceCapabilityController) error {
	var response struct {
		Data struct {
			Policy *deviceCapabilityPolicy `json:"device_capabilities"`
		} `json:"data"`
	}
	if controller == nil || json.Unmarshal(body, &response) != nil || response.Data.Policy == nil {
		return errors.New("runtime response has no device capability policy")
	}
	return controller.Apply(ctx, *response.Data.Policy)
}
