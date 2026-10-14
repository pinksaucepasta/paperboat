//go:build windows

package hostruntimecmd

import (
	"context"

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
		return runCarrierProcess(ctx, config, func() error { return nil }, func() error { return nil }, 0)
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
	return service.RunWindowsService(service.ServiceEntryConfig{Name: name, Executable: config.Executable, EnrolledSID: config.OwnerSID, Arguments: []string{"daemon", "__runtime-carrier", "--instance", config.Instance}, Environment: config.Environment})
}
