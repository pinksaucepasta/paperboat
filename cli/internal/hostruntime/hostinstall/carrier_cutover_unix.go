//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// StagedCandidate is what a build reports after staging its own carrier generation.
// The installing process (often the previous build's helper) reads the generation's
// own descriptor for the rest.
type StagedCandidate struct {
	Generation string `json:"generation"`
}

// stageUnixUpgrade has the build being installed stage its own session owner and,
// for a source upgrade of a serving carrier, its own carrier generation, through
// its own executable (stageCandidateByExecutable). Each build renders and checks
// only its own units, and the installing process is often another build: a signed
// update's helper is the previous build, and pb update --from runs the invoking CLI
// (pbvm1). nil means no carrier generation was staged.
func stageUnixUpgrade(ctx context.Context, request Request, paths installPaths, carrier bool) (*carrierUpgrade, error) {
	var retained CarrierConfig
	if carrier {
		var err error
		if retained, err = ReadCarrierConfig(ctx, paths.instance); err != nil {
			return nil, err
		}
		carrier = retained.Source != request.Source
	}
	staged, err := stageCandidateByExecutable(ctx, request, carrier)
	if err != nil || !carrier {
		return nil, err
	}
	generation, err := sourceGeneration(request.Source)
	if err != nil || staged.Generation != generation {
		return nil, errors.Join(ErrInvalidRequest, err)
	}
	candidate, err := readCarrierGenerationFile(ctx, paths.instance, generation)
	if err != nil {
		return nil, err
	}
	if candidate.Source != request.Source {
		return nil, ErrInvalidRequest
	}
	capability, err := ReadCarrierCapability(ctx, retained)
	if err != nil {
		return nil, err
	}
	defer clear(capability)
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_ready")
	return &carrierUpgrade{candidate: candidate, previous: retained, capability: append([]byte(nil), capability...)}, nil
}

// StageCandidate runs in the build being installed, as root: it starts its own
// session owner and, with carrier, its own carrier generation next to the serving
// one, rendered by this build, and waits until that generation serves.
func StageCandidate(ctx context.Context, request Request, carrier bool) (StagedCandidate, error) {
	if ctx == nil {
		return StagedCandidate{}, ErrInvalidRequest
	}
	if os.Geteuid() != 0 {
		return StagedCandidate{}, ErrNotPrivileged
	}
	if err := Validate(request, request.UID); err != nil {
		return StagedCandidate{}, err
	}
	if err := PrepareSessionOwner(ctx, request); err != nil || !carrier {
		return StagedCandidate{}, err
	}
	paths := platformPaths(request.UID)
	// The serving generation's unit is the previous build's render: only its descriptor is read here.
	retained, err := verifyCarrierDescriptor(readCarrierConfigFile(ctx, paths.instance))
	if err != nil {
		return StagedCandidate{}, err
	}
	capability, err := ReadCarrierCapability(ctx, retained)
	if err != nil {
		return StagedCandidate{}, err
	}
	defer clear(capability)
	candidate, err := prepareCarrierConfig(ctx, request)
	if err != nil {
		return StagedCandidate{}, err
	}
	// Endpoint and account custody cannot change during a source upgrade.
	candidate.Environment = retained.Environment
	stable := candidate
	stable.Source, stable.Generation, stable.Executable = retained.Source, retained.Generation, retained.Executable
	stable.ControlEndpoint, stable.LockPath = retained.ControlEndpoint, retained.LockPath
	want, _ := json.Marshal(stable)
	have, _ := json.Marshal(retained)
	if !bytes.Equal(want, have) {
		return StagedCandidate{}, ErrInvalidRequest
	}
	// Failures past this point are undone by the lifecycle rollback, which
	// removes the candidate generation by name (unixCarrierInstallation.Restore).
	if err := publishCarrierFiles(candidate, "", capability); err != nil {
		return StagedCandidate{}, err
	}
	next, err := carrierInstaller(candidate)
	if err != nil {
		return StagedCandidate{}, err
	}
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_start")
	if err := next.Install(ctx); err != nil {
		return StagedCandidate{}, err
	}
	if err := waitCarrierReady(ctx, candidate); err != nil {
		return StagedCandidate{}, err
	}
	return StagedCandidate{Generation: candidate.Generation}, nil
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
