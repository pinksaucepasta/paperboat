//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// A source upgrade starts the new carrier generation in its own unit and waits
// until it serves while the old generation keeps serving everything. The update
// then marks the new generation active and, once it commits, retires the old
// one, which finishes its open streams and exits (make-before-break). Prior art:
// Teleport graceful restart (lib/service/signals.go) and nginx binary upgrade
// (~/ref/nginx/src/os/unix/ngx_process_cycle.c). Nothing is refused meanwhile.
type unixCarrierUpgrade struct {
	candidate, previous CarrierConfig
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
	return &unixCarrierUpgrade{candidate: candidate, previous: retained}, nil
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
	if u == nil || failed {
		return nil
	}
	// The update is committed; reconciliation retries a retirement that fails here.
	if _, retireErr := retireCarrierGeneration(context.WithoutCancel(ctx), u.previous); retireErr != nil {
		diagnosticlog.TryWarn("carrier retirement deferred", "stage", "retire_previous")
	}
	return nil
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
