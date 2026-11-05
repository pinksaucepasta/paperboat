//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// sweepGenerations is the garbage collector of retired generations, run by the
// updater's reconciliation under the native installation lock (an update holds
// the same lock, so its staged candidate is never swept). Every carrier
// generation except activeCarrier is asked to retire while it runs and removed
// once it exited; every hostd generation except the installed one is removed
// once it exited after handing its listeners over. With stop (uninstall) every
// generation is stopped and removed at once.
func sweepGenerations(ctx context.Context, request Request, activeCarrier string, stop bool) error {
	paths := platformPaths(request.UID)
	var result error
	carriers, err := filepath.Glob(carrierGenerationPath(paths.instance, "*"))
	if err != nil {
		return err
	}
	for _, path := range carriers {
		generation := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "generation-"), ".json")
		if generation == activeCarrier {
			continue
		}
		c, err := readCarrierGenerationFile(ctx, paths.instance, generation)
		if err == nil {
			err = sweepCarrierGeneration(ctx, c, stop)
		}
		result = errors.Join(result, err)
	}
	active := ""
	if !stop {
		if active, err = sourceGeneration(request.Source); err != nil {
			return errors.Join(result, err)
		}
	}
	hostds, err := filepath.Glob(hostdGenerationDefinition(request.Platform, request.UID, "*"))
	if err != nil {
		return errors.Join(result, err)
	}
	for _, definition := range hostds {
		generation := hostdDefinitionGeneration(request.Platform, request.UID, definition)
		if generation == active || !sessionowner.ValidGeneration(generation) {
			continue
		}
		result = errors.Join(result, sweepHostdGeneration(ctx, request, generation, definition, stop))
	}
	return result
}

func sweepCarrierGeneration(ctx context.Context, c CarrierConfig, stop bool) error {
	if !stop {
		running, err := retireCarrierGeneration(ctx, c)
		if err != nil || running {
			return err
		}
	}
	return removeCarrierGeneration(ctx, c)
}

// removeCarrierGeneration stops and removes one generation's unit, then its
// own descriptor and lock. The account's capability is shared and stays.
func removeCarrierGeneration(ctx context.Context, c CarrierConfig) error {
	installer, err := carrierInstallerPending(c)
	if err != nil {
		return err
	}
	if err := installer.Uninstall(ctx); err != nil {
		return err
	}
	status, err := installerController(c).(service.NativeLifecycleController).Inspect(ctx, installer.DefinitionPath())
	if err != nil {
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

// sweepHostdGeneration removes a non-active hostd generation once it exited. A
// running one is still finishing work it accepted before its hand-over; it only
// loses its boot start.
func sweepHostdGeneration(ctx context.Context, request Request, generation, definition string, stop bool) error {
	controller, err := service.HostdController(request.Platform, generation, request.UID, service.ExecRunner{})
	if err != nil {
		return err
	}
	native := controller.(service.NativeLifecycleController)
	if !stop {
		status, err := native.Inspect(ctx, definition)
		if err != nil {
			return err
		}
		if status.Running {
			if status.Enabled && runtime.GOOS == "linux" {
				return native.Disable(ctx, definition)
			}
			return nil
		}
	}
	if err := native.Remove(ctx, definition); err != nil {
		return err
	}
	if err := os.Remove(definition); err != nil && !installNotExistOnly(err) {
		return err
	}
	return nil
}

// hostdGenerationDefinition is the native declaration of one hostd generation
// (service.HostdController names the same unit); "*" matches every generation.
func hostdGenerationDefinition(platform string, uid int, generation string) string {
	instance := "u" + strconv.Itoa(uid)
	if platform == "darwin" {
		return filepath.Join("/Library/LaunchDaemons", service.HostdLabel+"."+instance+"."+generation+".plist")
	}
	return filepath.Join("/etc/systemd/system", "paperboat-hostd-"+instance+"-"+generation+".service")
}

func hostdDefinitionGeneration(platform string, uid int, definition string) string {
	prefix, suffix, _ := strings.Cut(hostdGenerationDefinition(platform, uid, "*"), "*")
	return strings.TrimSuffix(strings.TrimPrefix(definition, prefix), suffix)
}
