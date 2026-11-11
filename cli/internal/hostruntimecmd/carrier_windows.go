//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

func carrierInstance(instance string) (string, error) {
	if _, err := hostinstall.WindowsInstanceRoot(instance); err != nil {
		return "", err
	}
	return instance, nil
}
func carrierHostdToken(config hostinstall.CarrierConfig) ([]byte, error) {
	return readWindowsHostdTokenForSID(config.HostdTokenFile, config.OwnerSID)
}

func runNativeCarrier(ctx context.Context, config hostinstall.CarrierConfig) error {
	sid, err := currentWindowsSID()
	if err != nil {
		return err
	}
	if sid == config.OwnerSID {
		err := runCarrierProcess(ctx, config, func() error { return nil }, func() error { return nil }, 0, nil)
		if err != nil && filepath.IsAbs(config.RuntimeRoot) {
			// The service that started this process sees only its exit code; keep the cause where hostd keeps its own.
			recordWindowsLaunchFailure(filepath.Join(config.RuntimeRoot, "carrier-startup-error.log"), err)
		}
		return err
	}
	if sid != "S-1-5-18" {
		return hostinstall.ErrInvalidRequest
	}
	if _, err := service.OwnedWindowsRoleExecutable(service.CarrierKind, config.Instance+"-"+config.Generation); err != nil {
		return err
	}
	name, err := service.WindowsCarrierName(config.Instance, config.Generation)
	if err != nil {
		return err
	}
	return service.RunWindowsService(service.ServiceEntryConfig{Name: name, Executable: config.Executable, EnrolledSID: config.OwnerSID, Arguments: []string{"daemon", "__runtime-carrier", "--instance", config.Instance, "--generation", config.Generation}, Environment: config.Environment,
		LaunchFailure: func(err error) { recordWindowsServiceLaunchFailure(name, err) },
		// The installer stops hostd, the local daemon and the updater and leaves this service running, so this is
		// the process that notices an installer that died after the stop. It also runs at boot: the service is
		// automatic. It ends only with the service.
		StartPrivilegedSidecar: func(ctx context.Context) (service.PrivilegedSidecar, error) {
			done := make(chan error, 1)
			go func() {
				windowsDeadInstaller(config.OwnerSID).watch(ctx, deadWriterPoll, deadWriterMaxWait, func(err error) {
					recordWindowsServiceLaunchFailure(name, runtimeFailure("recovering the interrupted installation failed", err))
				})
				done <- nil
			}()
			return service.PrivilegedSidecar{Done: done}, nil
		}})
}

// installerLockTry is how long one look waits for each install lock; a running installer holds them throughout.
const installerLockTry = 300 * time.Millisecond

func windowsDeadInstaller(ownerSID string) deadInstaller {
	// A lock that stays held past the try belongs to a running installer; any other failure is an error.
	try := func(ctx context.Context, lock func(context.Context) (func() error, error)) (func() error, error) {
		tryCtx, cancel := context.WithTimeout(ctx, installerLockTry)
		defer cancel()
		unlock, err := lock(tryCtx)
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, errInstallerLive
		}
		return unlock, err
	}
	return deadInstaller{
		pending: func() (bool, error) { return hostinstall.WindowsInstallInterrupted(ownerSID) },
		tryLock: func(ctx context.Context) (func() error, error) {
			// pb install, pb update and pb doctor --repair hold this one for their whole run.
			unlock, err := try(ctx, lockWindowsNativeInstall)
			if err != nil {
				return nil, err
			}
			// A signed update's activation holds only the publication lock: it is tried and released, because the
			// recovery takes it again itself. Only the install lock stays held for the recovery.
			publication, err := try(ctx, hostinstall.LockWindowsNativePublication)
			if err != nil {
				return nil, errors.Join(err, unlock())
			}
			return unlock, publication()
		},
		recover: func(ctx context.Context) (func(context.Context) error, error) {
			ctx, cancel := context.WithTimeout(ctx, elevation.RuntimeActivationDuration)
			defer cancel()
			// What pb doctor --repair runs first: the previous version's files and services, then the candidate.
			// finishSelf is this service's own stop or removal (the killed install had created or started it).
			finishSelf, err := recoverSuppliedWindowsInstallationWithin(ctx, ownerSID, false)
			if err != nil {
				return finishSelf, err
			}
			// The killed installer had moved the machine guard to its version (Linux: the updater's start heals it).
			instance, err := hostinstall.WindowsInstanceForSID(ownerSID)
			if err != nil {
				return finishSelf, err
			}
			installed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
			if runtimeCausesOnly(err, os.ErrNotExist, hostinstall.ErrNotInstalled) {
				return finishSelf, nil // the killed install was the first one: no installation is left to heal for
			}
			if err != nil {
				return finishSelf, err
			}
			layout, err := service.WindowsUserLayout(ownerSID)
			if err != nil {
				return finishSelf, err
			}
			_, err = hostinstall.HealMachineGuard(ctx, installed.Source, layout.Binary)
			return finishSelf, err
		},
	}
}
