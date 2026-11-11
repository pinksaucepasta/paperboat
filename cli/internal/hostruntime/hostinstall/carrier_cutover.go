//go:build linux || darwin || windows

package hostinstall

import (
	"context"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// A source upgrade starts the new carrier generation in its own unit or service as a
// standby: it prepares everything but the machine's network and waits until it
// answers. The update then marks it active and, once the new hostd and worker
// serve, hands the network over: the serving carrier releases it and exits, the
// candidate starts it on the same identity (switchNetwork). Only that switch
// pauses connections; sessions live in the session owners and the worker.
// Prior art: Tailscale restarts tailscaled with the same persisted node key
// (~/ref/tailscale/clientupdate/clientupdate.go restartSystemdUnit,
// control/controlclient/direct.go register reuses persist.PrivateNodeKey).
type carrierUpgrade struct {
	candidate, previous CarrierConfig
	capability          []byte
}

// switchNetwork hands the machine's network to the candidate: the serving
// carrier releases it (and exits), then the candidate starts it. A failure
// leaves the lifecycle rollback to restart the previous carrier.
func (u *carrierUpgrade) switchNetwork(ctx context.Context) error {
	if u == nil {
		return nil
	}
	previous, err := carriercontrol.NewClient(u.previous.ControlEndpoint, u.capability, u.previous.Source.SHA256, u.previous.OwnerSID)
	if err != nil {
		return err
	}
	next, err := carriercontrol.NewClient(u.candidate.ControlEndpoint, u.capability, u.candidate.Source.SHA256, u.candidate.OwnerSID)
	if err != nil {
		return err
	}
	started := time.Now()
	releaseErr := previous.ReleaseNetwork(ctx)
	diagnosticlog.TryInfo("carrier cutover", "stage", "network_released", "failed", releaseErr != nil, "duration_ms", time.Since(started).Milliseconds())
	if releaseErr != nil {
		return fmt.Errorf("releasing the network from the running carrier: %w", releaseErr)
	}
	startErr := next.StartNetwork(ctx)
	diagnosticlog.TryInfo("carrier cutover", "stage", "network_started", "failed", startErr != nil, "duration_ms", time.Since(started).Milliseconds())
	if startErr != nil {
		return fmt.Errorf("starting the network on the new carrier: %w", startErr)
	}
	return nil
}

// carrierServingWait is how long the commit waits for the new carrier to answer (a crashed one is restarted by its unit).
const carrierServingWait = 10 * time.Second

// confirmServing fails unless the new carrier answers within wait. It runs after the new daemon and the new
// services passed their checks, right before the update commits: a carrier that took the network and then went
// away fails the update here, while the previous version can still be restored.
func (u *carrierUpgrade) confirmServing(ctx context.Context, wait time.Duration) error {
	if u == nil {
		return nil
	}
	client, err := carriercontrol.NewClient(u.candidate.ControlEndpoint, u.capability, u.candidate.Source.SHA256, u.candidate.OwnerSID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		if err = client.Health(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return hostInstallFailure("the new version's carrier is not serving, so the update is not committed", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// activate marks the already serving candidate generation active: readers of
// the account's carrier descriptor address it from now on.
func (u *carrierUpgrade) activate() error {
	if u == nil {
		return nil
	}
	err := publishCarrierDescriptor(u.candidate)
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_active", "failed", err != nil)
	return err
}

// finish retires the previous generation once the update committed. A failed
// update was already rolled back to it, so it keeps serving untouched.
func (u *carrierUpgrade) finish(ctx context.Context, failed bool) error {
	if u == nil {
		return nil
	}
	clear(u.capability)
	if failed {
		return nil
	}
	// The update is committed; reconciliation retries a retirement that fails here.
	if _, retireErr := retireCarrierGeneration(context.WithoutCancel(ctx), u.previous); retireErr != nil {
		diagnosticlog.TryWarn("carrier retirement deferred", "stage", "retire_previous")
	}
	return nil
}

// startCarrierIfStopped restarts a carrier whose process exited after it
// released the machine's network, and waits until it serves again.
func startCarrierIfStopped(ctx context.Context, c CarrierConfig) error {
	installer, err := carrierInstaller(c)
	if err != nil {
		return err
	}
	native, ok := installerController(c).(service.NativeLifecycleController)
	if !ok {
		return service.ErrInvalidDefinition
	}
	status, err := native.Inspect(ctx, installer.DefinitionPath())
	if err != nil {
		return err
	}
	if !status.Running {
		if err := native.Start(ctx, installer.DefinitionPath()); err != nil {
			return err
		}
	}
	return waitCarrierReady(ctx, c)
}
