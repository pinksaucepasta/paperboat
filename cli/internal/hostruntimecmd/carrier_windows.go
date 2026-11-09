//go:build windows

package hostruntimecmd

import (
	"context"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
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
	if _, err := service.OwnedWindowsRoleExecutable(service.CarrierKind, config.Instance); err != nil {
		return err
	}
	name, err := service.WindowsCarrierName(config.Instance)
	if err != nil {
		return err
	}
	return service.RunWindowsService(service.ServiceEntryConfig{Name: name, Executable: config.Executable, EnrolledSID: config.OwnerSID, Arguments: []string{"daemon", "__runtime-carrier", "--instance", config.Instance}, Environment: config.Environment,
		LaunchFailure: func(err error) { recordWindowsServiceLaunchFailure(name, err) }})
}
