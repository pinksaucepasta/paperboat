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

func stageUnixCarrierUpgrade(ctx context.Context, request Request, paths installPaths) (_ *carrierUpgrade, resultErr error) {
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
	return &carrierUpgrade{candidate: candidate, previous: retained, capability: append([]byte(nil), capability...)}, nil
}

// retireCarrierGeneration stops a non-active generation from starting at boot and
// asks a process still running to exit (service.QuitCarrier).
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
