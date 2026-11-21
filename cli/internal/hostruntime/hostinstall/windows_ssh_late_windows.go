//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
)

var installedWindowsLateSSH windowsLateSSH

// CompleteSkippedWindowsSSH is run by the updater (LocalSystem) on its reconcile tick: within WindowsLateSSHWindow of
// an installation that skipped managed SSH, it prepares the installed generation's sshd as installation would have.
// It holds the installation and publication locks in the order installation takes them and rereads the installed
// state under them, so an update that replaced the generation meanwhile is never prepared over.
func CompleteSkippedWindowsSSH(ctx context.Context, instance string) error {
	config, err := LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil || !config.Committed || config.EnrollmentPending || len(config.Source.SHA256) < 32 {
		return nil // nothing installed and enrolled to complete; the carrier reconcile reports install faults
	}
	serviceName := windowsopenssh.ServiceName + "-" + instance + "-" + config.Source.SHA256[:32]
	present := func(context.Context) bool { return windowsServiceRunning(serviceName) }
	prepare := func(ctx context.Context) (resultErr error) {
		unlockInstall, err := LockWindowsNativeInstall(ctx)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, unlockInstall()) }()
		unlockPublication, err := LockWindowsNativePublication(ctx)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, unlockPublication()) }()
		current, err := LoadWindowsRuntimeConfigForInstance(instance)
		if err != nil || current.Source != config.Source || !current.Committed || current.EnrollmentPending || present(ctx) {
			return err // replaced, uncommitted or completed meanwhile: the next tick reads the new state
		}
		layout, err := service.WindowsUserLayout(current.OwnerSID)
		if err != nil {
			return err
		}
		request := windowsRepairRequest(current)
		request.Executable, request.WorkspaceRoot = layout.Binary, current.Workspace
		return installWindowsSSHAfterActivation(ctx, request, layout)
	}
	return installedWindowsLateSSH.complete(ctx, time.Now(), config.InstalledAt, present, prepare)
}

// windowsServiceRunning reports whether serviceName is registered and running; a generation descriptor without its
// running service is not managed SSH the worker can serve.
func windowsServiceRunning(serviceName string) bool {
	manager, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(serviceName)
	if err != nil {
		return false
	}
	defer item.Close()
	status, err := item.Query()
	return err == nil && status.State == svc.Running
}
