//go:build windows

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// stageWindowsCarrierUpgrade starts request's carrier generation beside the serving one, before the cutover: as a
// standby it prepares nothing that touches the machine's state or network until switchNetwork (Linux and macOS:
// stageUnixCarrierUpgrade). nil means nothing to stage: no carrier yet (the activation creates the first), or the
// serving one already runs request's source.
func stageWindowsCarrierUpgrade(ctx context.Context, request Request) (*carrierUpgrade, error) {
	instance, err := WindowsInstanceForSID(request.OwnerSID)
	if err != nil {
		return nil, err
	}
	retained, err := ReadCarrierConfig(ctx, instance)
	if installNotExistOnly(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if retained.Source.SHA256 == request.Source.SHA256 && retained.Source.Length == request.Source.Length {
		return nil, nil
	}
	// The serving carrier releases the network to the candidate later: it must serve now.
	if err := EnsureExistingCarrier(ctx, retained); err != nil {
		return nil, err
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
	// Failures past this point are undone by the lifecycle rollback, which removes every generation but the
	// baseline's (restoreWindowsCarrierSnapshot).
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

// retireCarrierGeneration stops a non-active generation: a standby that never took the network, or one that
// released it and is still exiting. It reports whether the process still runs. Repeating it is harmless.
func retireCarrierGeneration(ctx context.Context, c CarrierConfig) (bool, error) {
	installer, err := carrierInstallerPending(c)
	if err != nil {
		return false, err
	}
	native := service.WindowsController{}
	status, err := native.Inspect(ctx, installer.DefinitionPath())
	if err != nil || !status.Running {
		return false, err
	}
	return false, native.Stop(ctx, installer.DefinitionPath())
}

// removeWindowsCarrierGeneration stops and deletes one generation's service, then its own descriptor and lock. The
// account's capability is shared and stays.
func removeWindowsCarrierGeneration(ctx context.Context, c CarrierConfig) error {
	if err := verifyWindowsSource(ctx, c.Executable, c.Source, c.OwnerSID); err != nil {
		return err
	}
	installer, err := carrierInstaller(c)
	if err != nil {
		return err
	}
	native := service.WindowsController{}
	status, err := native.Inspect(ctx, installer.DefinitionPath())
	if err != nil {
		return err
	}
	if status.Registered || status.Running {
		if err := verifyCarrierNative(c, installer); err != nil {
			return err
		}
		if err := installer.Uninstall(ctx); err != nil {
			return err
		}
	}
	if status, err = native.Inspect(ctx, installer.DefinitionPath()); err != nil {
		return err
	}
	if status.Registered || status.Running {
		return service.ErrLifecycleUncertain
	}
	for _, path := range []string{carrierGenerationPath(c.Instance, c.Generation), c.LockPath} {
		if err := os.Remove(path); err != nil && !installNotExistOnly(err) {
			return err
		}
	}
	return nil
}

// windowsCarrierGenerations reads every carrier generation's own descriptor of the account.
func windowsCarrierGenerations(ctx context.Context, instance string) ([]CarrierConfig, error) {
	paths, err := filepath.Glob(carrierGenerationPath(instance, "*"))
	if err != nil {
		return nil, err
	}
	generations := make([]CarrierConfig, 0, len(paths))
	var result error
	for _, path := range paths {
		generation := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "generation-"), ".json")
		c, err := readCarrierGenerationFile(ctx, instance, generation)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		generations = append(generations, c)
	}
	return generations, result
}

// sweepWindowsCarrierGenerations retires and removes every carrier generation but active, under the native
// installation lock (an update holds it, so its staged candidate is never swept). With stop (uninstall) every
// generation goes.
func sweepWindowsCarrierGenerations(ctx context.Context, instance, active string) error {
	generations, result := windowsCarrierGenerations(ctx, instance)
	for _, c := range generations {
		if c.Generation == active {
			continue
		}
		running, err := retireCarrierGeneration(ctx, c)
		if err == nil && !running {
			err = removeWindowsCarrierGeneration(ctx, c)
		}
		result = errors.Join(result, err)
	}
	return result
}

// removeWindowsHostdGenerations removes the account's hostd generation services other than keep (registered or only
// declared). With onlyStopped a running generation stays: it is still serving or finishing work.
func removeWindowsHostdGenerations(ctx context.Context, instance, keep string, onlyStopped bool) error {
	names, err := windowsHostdGenerationNames(ctx, instance)
	if err != nil {
		return err
	}
	kept, err := service.WindowsHostdName(instance, keep)
	if err != nil {
		return err
	}
	var result error
	for _, name := range names {
		if strings.EqualFold(name, kept) {
			continue
		}
		definition := filepath.Join(WindowsProgramDataRoot(), "services", name+".json")
		if onlyStopped {
			status, err := (service.WindowsController{}).Inspect(ctx, definition)
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			if status.Running {
				continue
			}
		}
		if err := (service.WindowsController{}).Remove(ctx, definition); err != nil {
			result = errors.Join(result, err)
			continue
		}
		result = errors.Join(result, removeInstallFile(definition))
	}
	return result
}
