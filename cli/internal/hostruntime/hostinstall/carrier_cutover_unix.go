//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// A source upgrade starts the new carrier generation in its own unit as a
// standby: it prepares everything but the machine's network and waits until it
// answers. The update then marks it active and, once the new hostd and worker
// serve, hands the network over: the serving carrier releases it and exits, the
// candidate starts it on the same identity (switchNetwork). Only that switch
// pauses connections; sessions live in the session owners and the worker.
// Prior art: Tailscale restarts tailscaled with the same persisted node key
// (~/ref/tailscale/clientupdate/clientupdate.go restartSystemdUnit,
// control/controlclient/direct.go register reuses persist.PrivateNodeKey).
type unixCarrierUpgrade struct {
	candidate, previous CarrierConfig
	terminals           *terminalHandover
	capability          []byte
}

func stageUnixCarrierUpgrade(ctx context.Context, request Request, paths installPaths) (_ *unixCarrierUpgrade, resultErr error) {
	retained, err := ReadCarrierConfig(ctx, paths.instance)
	if err != nil {
		return nil, err
	}
	if retained.Source == request.Source {
		return nil, nil
	}
	capability, err := ReadCarrierCapability(ctx, retained)
	if err != nil {
		return nil, err
	}
	defer clear(capability)
	candidate, err := prepareCarrierConfig(ctx, request)
	if err != nil {
		return nil, err
	}
	// Endpoint and account custody cannot change during a source upgrade.
	candidate.Environment = retained.Environment
	stable := candidate
	stable.Source, stable.Generation, stable.Executable = retained.Source, retained.Generation, retained.Executable
	stable.ControlEndpoint, stable.LockPath = retained.ControlEndpoint, retained.LockPath
	want, _ := json.Marshal(stable)
	have, _ := json.Marshal(retained)
	if !bytes.Equal(want, have) {
		return nil, ErrInvalidRequest
	}
	// Failures past this point are undone by the lifecycle rollback, which
	// removes the candidate generation (unixCarrierInstallation.Restore).
	if err := publishCarrierFiles(candidate, "", capability); err != nil {
		return nil, err
	}
	next, err := carrierInstaller(candidate)
	if err != nil {
		return nil, err
	}
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_start")
	if err := next.Install(ctx); err != nil {
		return nil, err
	}
	if err := waitCarrierReady(ctx, candidate); err != nil {
		return nil, err
	}
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_ready")
	client, err := carriercontrol.NewClient(retained.ControlEndpoint, capability, retained.Source.SHA256)
	if err != nil {
		return nil, err
	}
	terminals, moveErr := stageTerminalHandover(ctx, request, retained, client)
	if moveErr != nil {
		diagnosticlog.TryWarn("terminal handover unavailable", "fallback", "reconnect")
	}
	return &unixCarrierUpgrade{candidate: candidate, previous: retained, terminals: terminals, capability: append([]byte(nil), capability...)}, nil
}

// switchNetwork hands the machine's network to the candidate: the serving
// carrier releases it (and exits), then the candidate starts it. A failure
// leaves the lifecycle rollback to restart the previous carrier.
func (u *unixCarrierUpgrade) switchNetwork(ctx context.Context) error {
	if u == nil {
		return nil
	}
	defer clear(u.capability)
	previous, err := carriercontrol.NewClient(u.previous.ControlEndpoint, u.capability, u.previous.Source.SHA256)
	if err != nil {
		return err
	}
	next, err := carriercontrol.NewClient(u.candidate.ControlEndpoint, u.capability, u.candidate.Source.SHA256)
	if err != nil {
		return err
	}
	started := time.Now()
	releaseErr := previous.ReleaseNetwork(ctx)
	diagnosticlog.TryInfo("carrier cutover", "stage", "network_released", "failed", releaseErr != nil, "duration_ms", time.Since(started).Milliseconds())
	if releaseErr != nil {
		return releaseErr
	}
	startErr := next.StartNetwork(ctx)
	diagnosticlog.TryInfo("carrier cutover", "stage", "network_started", "failed", startErr != nil, "duration_ms", time.Since(started).Milliseconds())
	return startErr
}

// activate marks the already serving candidate generation active: readers of
// the account's carrier descriptor address it from now on.
func (u *unixCarrierUpgrade) activate() error {
	if u == nil {
		return nil
	}
	err := publishCarrierDescriptor(u.candidate)
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_active", "failed", err != nil)
	return err
}

// finish retires the previous generation once the update committed. A failed
// update was already rolled back to it, so it keeps serving untouched.
func (u *unixCarrierUpgrade) finish(ctx context.Context, failed bool) error {
	if u == nil {
		return nil
	}
	clear(u.capability)
	err := u.terminals.finish(ctx, failed)
	if failed {
		return err
	}
	// The update is committed; reconciliation retries a retirement that fails here.
	if _, retireErr := retireCarrierGeneration(context.WithoutCancel(ctx), u.previous); retireErr != nil {
		diagnosticlog.TryWarn("carrier retirement deferred", "stage", "retire_previous")
	}
	return err
}

// retireCarrierGeneration stops a non-active generation from starting at boot and
// asks its running process to finish its open streams and exit (service.QuitCarrier).
// It reports whether the process still runs. Repeating it is harmless.
func retireCarrierGeneration(ctx context.Context, c CarrierConfig) (bool, error) {
	installer, err := carrierInstallerPending(c)
	if err != nil {
		return false, err
	}
	controller := installerController(c)
	native, ok := controller.(service.NativeLifecycleController)
	if !ok {
		return false, service.ErrInvalidDefinition
	}
	status, err := native.Inspect(ctx, installer.DefinitionPath())
	if err != nil {
		return false, err
	}
	// launchd's disable is a bootout, which would cut open streams; a retired
	// generation started at boot exits on the next reconciliation's signal.
	if status.Enabled && runtime.GOOS == "linux" {
		if err := native.Disable(ctx, installer.DefinitionPath()); err != nil {
			return status.Running, err
		}
	}
	if !status.Running {
		return false, nil
	}
	return true, service.QuitCarrier(ctx, controller)
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
