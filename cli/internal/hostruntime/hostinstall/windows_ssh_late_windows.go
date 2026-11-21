//go:build windows

package hostinstall

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
)

var installedWindowsLateSSH windowsLateSSH

// CompleteSkippedWindowsSSH is run by the updater (LocalSystem) on its reconcile tick: within WindowsLateSSHWindow of
// an installation that skipped managed SSH, it prepares the installed generation's sshd as installation would have,
// under the native installation lock. The worker serves the generation once its descriptor exists.
func CompleteSkippedWindowsSSH(ctx context.Context, instance string) error {
	config, err := LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil || !config.Committed || config.EnrollmentPending || len(config.Source.SHA256) < 32 {
		return nil // nothing installed and enrolled to complete; the carrier reconcile reports install faults
	}
	present := func(ctx context.Context) bool {
		_, err := windowsopenssh.LoadGeneration(ctx, config.OwnerSID, config.Source.SHA256[:32])
		return err == nil
	}
	prepare := func(ctx context.Context) error {
		layout, err := service.WindowsUserLayout(config.OwnerSID)
		if err != nil {
			return err
		}
		unlock, err := LockWindowsNativeInstall(ctx)
		if err != nil {
			return err
		}
		defer unlock()
		request := windowsRepairRequest(config)
		request.Executable, request.WorkspaceRoot = layout.Binary, config.Workspace
		return installWindowsSSHAfterActivation(ctx, request, layout)
	}
	return installedWindowsLateSSH.complete(ctx, time.Now(), config.InstalledAt, present, prepare)
}
