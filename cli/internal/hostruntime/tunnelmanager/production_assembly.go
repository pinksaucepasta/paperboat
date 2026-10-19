package tunnelmanager

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/networkrecovery"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/networkmonitor"
)

func (a *ProductionAssembly) Shutdown(ctx context.Context) error {
	if a == nil || a.Manager == nil || ctx == nil {
		return ErrProductionAssemblyInvalid
	}
	a.controlMu.Lock()
	cancel, done := a.controlCancel, a.controlDone
	a.controlMu.Unlock()
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
	var controlCleanupErr error
	a.controlMu.Lock()
	controlErr := a.controlErr
	a.controlMu.Unlock()
	if failure, ok := controlErr.(controlTerminationFailure); ok {
		// This final attempt was not sent to the reconnect observer. Retain
		// independent cleanup failures for the component lifecycle owner.
		if fault := errorreport.ProjectFault(ctx, "paperboat-daemon", "tunnel", "control_request", "control_request_failed", failure); fault.Outcome != "canceled" {
			controlCleanupErr = failure
		}
	}
	var recoveryErr error
	if a.networkRecovery != nil {
		recoveryErr = a.networkRecovery.Shutdown(ctx)
	}
	return errors.Join(controlCleanupErr, recoveryErr, a.Manager.Shutdown(ctx))
}

// HandleNetworkEvent connects the platform network monitor to the durable
// tunnel recovery coordinator. It is intentionally a no-op before Start or
// after Shutdown, so a late monitor callback cannot stage a carrier during
// lifecycle teardown.
func (a *ProductionAssembly) HandleNetworkEvent(event networkmonitor.Event) {
	if a == nil || a.networkRecovery == nil {
		return
	}
	a.networkRecovery.HandleNetworkEvent(event)
}

func (a *ProductionAssembly) NetworkHealth(tunnelID string) (networkrecovery.HealthSnapshot, bool) {
	if a == nil || a.networkRecovery == nil {
		return networkrecovery.HealthSnapshot{}, false
	}
	return a.networkRecovery.Health(tunnelID)
}

func (a *ProductionAssembly) NetworkHealthSnapshots() map[string]networkrecovery.HealthSnapshot {
	if a == nil || a.networkRecovery == nil {
		return nil
	}
	return a.networkRecovery.HealthSnapshots()
}

func (a *ProductionAssembly) ResourceCounts() map[string]uint64 {
	if a == nil || a.Manager == nil {
		return nil
	}
	return a.Manager.ResourceCounts()
}

func (a *ProductionAssembly) WorkloadIdentities() []string {
	if a == nil || a.Manager == nil {
		return nil
	}
	return a.Manager.WorkloadIdentities()
}

var _ interface {
	Start(context.Context) error
	Shutdown(context.Context) error
	ResourceCounts() map[string]uint64
} = (*ProductionAssembly)(nil)

// ConnectorStatus describes only published carriers that are currently usable.
func (a *ProductionAssembly) ConnectorStatus() connector.Status {
	var result connector.Status
	if a == nil || a.Manager == nil || a.Manager.Manager == nil {
		return result
	}
	for _, active := range a.Manager.Manager.ActiveSnapshot() {
		provider, ok := active.(ActiveCarrierProvider)
		if !ok || provider.ActiveDataCarrier() == nil {
			continue
		}
		carrier := provider.ActiveDataCarrier()
		pool := carrier.Pool()
		if pool == nil || pool.State() != connector.DataCarrierPoolReady {
			continue
		}
		var transport connector.Transport
		for _, current := range carrier.Snapshot() {
			if current.State == connector.DataCarrierReady {
				transport = current.Transport
				break
			}
		}
		if transport == "" || pool.State() != connector.DataCarrierPoolReady {
			continue
		}
		result.Connected = true
		if active.Generation() >= result.Generation {
			result.Generation = active.Generation()
			result.Transport = transport
		}
	}
	return result
}
