//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"golang.org/x/sys/windows"
)

func pendingWindowsDaemonInstaller(layout service.Layout, executable string) (*service.Installer, error) {
	return service.New(service.Config{Platform: "windows", Kind: service.DaemonKind, Instance: layout.Instance, ConfigRoot: filepath.Dir(layout.UpdateStateRoot), Executable: executable, User: "Paperboat", Group: "Paperboat", Arguments: []string{"daemon", "__runtime-local-daemon", "--instance", layout.Instance}, Controller: service.WindowsController{}})
}

// PreparePendingWindowsDaemonReplacement stops the exact protected pending
// declaration before carrier preflight. Its caller restores it on failure.
func PreparePendingWindowsDaemonReplacement(ctx context.Context, ownerSID string) (func() error, error) {
	instance, err := WindowsInstanceForSID(ownerSID)
	if err != nil {
		return nil, err
	}
	previous, err := LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return nil, err
	}
	if !previous.EnrollmentPending || previous.OwnerSID != ownerSID {
		return nil, ErrInvalidRequest
	}
	layout, err := WindowsLayoutForInstance(instance)
	if err != nil {
		return nil, err
	}
	executable, err := WindowsPinnedSourcePath(layout, previous.Source)
	if err != nil {
		return nil, err
	}
	if err := previous.Source.Verify(executable); err != nil {
		return nil, err
	}
	installer, err := pendingWindowsDaemonInstaller(layout, executable)
	if err != nil {
		return nil, err
	}
	if err := installer.VerifyPublishedDefinition(); err != nil {
		return nil, err
	}
	controller := service.WindowsController{}
	before, err := controller.Inspect(ctx, installer.DefinitionPath())
	if err != nil {
		return nil, err
	}
	if !before.Registered || !before.Enabled {
		return nil, service.ErrLifecycleUncertain
	}
	resume := func() error {
		if !before.Running {
			return nil
		}
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), windowsServicePlanRollbackTimeout)
		defer cancel()
		if err := controller.Start(recovery, installer.DefinitionPath()); err != nil {
			return err
		}
		return waitWindowsLocalDaemonReady(recovery, ownerSID, previous.StateRoot)
	}
	if err := controller.Stop(ctx, installer.DefinitionPath()); err != nil {
		return nil, errors.Join(err, resume())
	}
	return resume, nil
}

func readWindowsLocalDaemonSnapshot(ctx context.Context, ownerSID, stateRoot string) (localapi.Snapshot, error) {
	paths, err := localapi.WindowsPaths(stateRoot, ownerSID)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return localapi.Snapshot{}, ErrNotPrivileged
	}
	caller := user.User.Sid.String()
	if caller == "S-1-5-18" {
		return localapi.ReadSystemOwnerSnapshot(ctx, paths.SocketPath, ownerSID, 2*time.Second)
	}
	if caller != ownerSID {
		return localapi.Snapshot{}, ErrNotPrivileged
	}
	client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	return client.Snapshot(ctx)
}

func waitWindowsLocalDaemonVersion(ctx context.Context, request Request) error {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		if err := waitWindowsLocalDaemonReady(bounded, request.OwnerSID, request.StateRoot); err != nil {
			return err
		}
		state, err := readWindowsLocalDaemonSnapshot(bounded, request.OwnerSID, request.StateRoot)
		if err == nil && state.DaemonVersion == request.Source.Version && state.DaemonState == "ready" {
			return nil
		}
		last = err
		select {
		case <-bounded.Done():
			return errors.Join(service.ErrLifecycleNotReady, last, bounded.Err())
		case <-tick.C:
		}
	}
}

// A stable declaration can still have an older loaded child after an interrupted
// fixed-slot replacement. Restart only that mismatched owner, with ordinary
// workload admission; a matching daemon keeps its live connections.
func prepareWindowsLoadedLocalDaemonRepair(ctx context.Context, request Request, layout service.Layout) (func() error, error) {
	lockPath, err := windowsLocalDaemonLockPath(request.StateRoot)
	if err != nil {
		return nil, err
	}
	running, err := localdaemon.WindowsOwnerServiceRunning(lockPath, request.OwnerSID)
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, nil
	}
	state, err := readWindowsLocalDaemonSnapshot(ctx, request.OwnerSID, request.StateRoot)
	if err != nil {
		return nil, err
	}
	if state.DaemonVersion == request.Source.Version {
		return nil, nil
	}
	for _, machine := range state.Machines {
		if machine.ActiveConsumers != 0 {
			return nil, hostdproto.ErrMaintenanceBusy
		}
	}
	root, err := WindowsInstanceRoot(layout.Instance)
	if err != nil {
		return nil, err
	}
	abort, err := PrepareSuppliedMaintenance(ctx, layout.HostdSocket, filepath.Join(root, "hostd.token"), false)
	if err != nil {
		return nil, err
	}

	_, _, daemon, err := windowsRoleInstallers(layout, false)
	if err != nil {
		abort()
		return nil, err
	}
	controller := service.WindowsController{}
	// windowsRoleInstallers returns hostd, updater, daemon in that order and
	// the controller validates this exact owned declaration before stopping it.
	before, err := controller.Inspect(ctx, daemon.DefinitionPath())
	if err != nil || !before.Registered || !before.Enabled || !before.Running || !before.Ready {
		abort()
		return nil, errors.Join(service.ErrLifecycleUncertain, err)
	}
	resume := func() error {
		defer abort()
		recovery, cancel := context.WithTimeout(context.Background(), windowsServicePlanRollbackTimeout)
		defer cancel()
		if err := controller.Start(recovery, daemon.DefinitionPath()); err != nil {
			return err
		}
		return waitWindowsLocalDaemonVersion(recovery, request)
	}
	if err := controller.Stop(ctx, daemon.DefinitionPath()); err != nil {
		return nil, errors.Join(err, resume())
	}
	// The caller restores the fixed slot before its role plan starts this owner.
	// Keep admission closed until that plan or bounded failure recovery finishes.
	return resume, nil
}
