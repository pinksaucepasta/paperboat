//go:build windows

package hostruntimecmd

import (
	"context"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

// EnrolledInstallation reports whether the current user has a paired Paperboat installation: the one thing the
// elevated repair restores. It reads the installation record, which the user may read, and asks for no approval.
func EnrolledInstallation() (bool, error) {
	sid, err := currentWindowsSID()
	if err != nil {
		return false, err
	}
	instance, err := hostinstall.WindowsInstanceForSID(sid)
	if err != nil {
		return false, err
	}
	installed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if runtimeCausesOnly(err, os.ErrNotExist, hostinstall.ErrNotInstalled) {
		return false, nil
	}
	return err == nil && !installed.EnrollmentPending, err
}

func RepairManagedHost(ctx context.Context) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return elevation.RunRuntimeService(ctx, executable, elevation.ActionRepair, nil)
}
