//go:build windows

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
	"golang.org/x/sys/windows"
)

// runServiceCommand is the sole privileged command bridge used by the MSI,
// elevated setup engine, and repair flow. Every operation is idempotent and
// decodes the same bounded installation request before changing SCM state.
func runServiceCommand(ctx context.Context, args []string, stdin io.Reader, _, _ io.Writer) error {
	if len(args) > 0 && args[0] == elevation.BridgeCommand {
		return runServiceBridge(ctx, args[1:])
	}
	if len(args) != 1 {
		return errors.New("service requires install, commit, uninstall, uninstall-persisted, purge, repair, or stop")
	}
	switch args[0] {
	case "uninstall-persisted":
		if !elevation.IsCurrentProcessElevated() {
			return runElevatedServiceOperation(ctx, elevation.ActionUninstallPersist)
		}
		ownerSID, err := currentWindowsSID()
		if err != nil {
			return err
		}
		return uninstallPersistedWindowsRuntime(ctx, ownerSID)
	case "purge":
		if !elevation.IsCurrentProcessElevated() {
			return runElevatedServiceOperation(ctx, elevation.ActionPurge)
		}
		ownerSID, err := currentWindowsSID()
		if err != nil {
			return err
		}
		return hostinstall.Purge(ctx, ownerSID)
	case "repair":
		if !elevation.IsCurrentProcessElevated() {
			return runElevatedServiceOperation(ctx, elevation.ActionRepair)
		}
		ownerSID, err := currentWindowsSID()
		if err != nil {
			return err
		}
		return repairWindowsInstallation(ctx, ownerSID)
	case "stop":
		if !elevation.IsCurrentProcessElevated() {
			return runElevatedServiceOperation(ctx, elevation.ActionStop)
		}
		ownerSID, err := currentWindowsSID()
		if err != nil {
			return err
		}
		return hostinstall.Stop(ctx, ownerSID)
	case "install", "commit", "uninstall":
		request, err := hostinstall.Decode(stdin)
		if err != nil {
			return err
		}
		if !elevation.IsCurrentProcessElevated() {
			action := map[string]string{"install": elevation.ActionInstall, "commit": elevation.ActionCommit, "uninstall": elevation.ActionUninstall}[args[0]]
			return runElevatedServiceOperation(ctx, action, request)
		}
		switch args[0] {
		case "install":
			return hostinstall.Install(ctx, request)
		case "commit":
			return hostinstall.Commit(request)
		default:
			return uninstallWindowsRuntime(ctx, request)
		}
	default:
		return errors.New("service requires install, commit, uninstall, uninstall-persisted, purge, repair, or stop")
	}
}

func runElevatedServiceOperation(ctx context.Context, action string, payload ...any) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	var value any
	if len(payload) != 0 {
		value = payload[0]
	}
	return elevation.RunRuntimeService(ctx, executable, action, value)
}

func runServiceBridge(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("__runtime-service bridge", flag.ContinueOnError)
	requestPath := flags.String("request-file", "", "protected request file")
	resultPath := flags.String("result-file", "", "protected result file")
	cancelPath := flags.String("cancel-file", "", "protected cancellation marker")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *requestPath == "" || *resultPath == "" || *cancelPath == "" {
		return errors.New("service bridge requires request-file, result-file, and cancel-file")
	}
	return elevation.Execute(ctx, *requestPath, *resultPath, *cancelPath, dispatchElevatedOperation)
}

func dispatchElevatedOperation(ctx context.Context, request elevation.Request) error {
	switch request.Operation {
	case elevation.OperationRuntimeService:
		switch request.Action {
		case elevation.ActionUninstallPersist:
			return uninstallPersistedWindowsRuntime(ctx, request.OwnerSID)
		case elevation.ActionPurge:
			return hostinstall.Purge(ctx, request.OwnerSID)
		case elevation.ActionRepair:
			return repairWindowsInstallation(ctx, request.OwnerSID)
		case elevation.ActionStop:
			return hostinstall.Stop(ctx, request.OwnerSID)
		case elevation.ActionInstall, elevation.ActionInstallCommit, elevation.ActionCommit, elevation.ActionUninstall:
			installRequest, err := hostinstall.Decode(bytes.NewReader(request.Payload))
			if err != nil {
				return err
			}
			if !strings.EqualFold(request.OwnerSID, installRequest.OwnerSID) {
				return errors.New("elevated Windows request owner does not match installation owner")
			}
			var unlock func()
			if request.Action == elevation.ActionInstall || request.Action == elevation.ActionInstallCommit {
				unlock, err = lockWindowsNativeInstall(ctx)
				if err != nil {
					return err
				}
				defer unlock()
			}
			switch request.Action {
			case elevation.ActionInstall:
				return installWindowsRuntimeFromSuppliedBytes(ctx, installRequest)
			case elevation.ActionCommit:
				return hostinstall.Commit(installRequest)
			case elevation.ActionUninstall:
				return uninstallWindowsRuntime(ctx, installRequest)
			case elevation.ActionInstallCommit:
				if err := installWindowsRuntimeFromSuppliedBytes(ctx, installRequest); err != nil {
					return err
				}
				if err := hostinstall.Commit(installRequest); err != nil {
					return errors.Join(err, hostinstall.Uninstall(ctx, installRequest))
				}
				// Service registration can succeed before the enrolled-owner
				// workload finishes startup. Check hostd after the durable commit;
				// a stopped first launch may recover when started with final state.
				// Leave the installation intact on failure so pairing can resume.
				if err := hostinstall.EnsureCommittedWindowsHostdReady(ctx, installRequest); err != nil {
					return fmt.Errorf("Paperboat host service did not become ready after installation: %w", err)
				}
				return nil
			}
		}
	case elevation.OperationOpenSSH:
		config := windowsopenssh.DefaultConfig(nil)
		config.OwnerSID = request.OwnerSID
		switch request.Action {
		case elevation.ActionOpenSSHSetup:
			_, err := windowsopenssh.Setup(ctx, config)
			return err
		case elevation.ActionOpenSSHRepair:
			_, err := windowsopenssh.Repair(ctx, config)
			return err
		case elevation.ActionOpenSSHRemove:
			return windowsopenssh.RemovePaperboatState(ctx, config)
		}
	}
	return errors.New("unsupported elevated Windows operation")
}

func installWindowsRuntimeFromSuppliedBytes(ctx context.Context, request hostinstall.Request) error {
	instance, err := hostinstall.WindowsInstanceForSID(request.OwnerSID)
	if err != nil {
		return err
	}
	previous, loadErr := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	restoreServices := func() error {
		if loadErr != nil {
			return nil
		}
		if previous.SetupMode == "awaiting_enrollment" {
			return hostinstall.EnsureWindowsLocalDaemonService(context.Background(), previous.OwnerSID)
		}
		return hostinstall.Repair(context.Background(), previous.OwnerSID)
	}
	if loadErr == nil {
		if err := hostinstall.Stop(ctx, request.OwnerSID); err != nil {
			return err
		}
	}
	restoreJournal, err := updated.PrepareWindowsNativeInstall(ctx, request.OwnerSID)
	if err != nil {
		return errors.Join(fmt.Errorf("prepare Windows updater for supplied install: %w", err), restoreServices())
	}
	if err := hostinstall.Install(ctx, request); err != nil {
		return errors.Join(fmt.Errorf("install supplied Windows runtime: %w", err), restoreJournal(), restoreServices())
	}
	return nil
}

func lockWindowsNativeInstall(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, errors.New("nil Windows native install context")
	}
	name, err := windows.UTF16PtrFromString(`Global\PaperboatNativeInstall`)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, err
	}
	if handle == 0 {
		return nil, errors.New("create Windows native install mutex")
	}
	for {
		if err := ctx.Err(); err != nil {
			windows.CloseHandle(handle)
			return nil, err
		}
		state, waitErr := windows.WaitForSingleObject(handle, uint32((100 * time.Millisecond).Milliseconds()))
		if waitErr != nil {
			windows.CloseHandle(handle)
			return nil, waitErr
		}
		if state == windows.WAIT_OBJECT_0 || state == windows.WAIT_ABANDONED {
			return func() { _ = windows.ReleaseMutex(handle); _ = windows.CloseHandle(handle) }, nil
		}
		if state != uint32(windows.WAIT_TIMEOUT) {
			windows.CloseHandle(handle)
			return nil, errors.New("wait for Windows native install mutex")
		}
	}
}

func repairWindowsInstallation(ctx context.Context, ownerSID string) error {
	err := hostinstall.Repair(ctx, ownerSID)
	if errors.Is(err, hostinstall.ErrNotInstalled) {
		return nil
	}
	// hostinstall.Repair owns the complete role-scoped service contract. In
	// particular, Client repair removes PaperboatSshd while Host repair restores
	// it. A second unconditional OpenSSH repair here used to recreate the SSH
	// service on Client installations and contradicted that persisted role.
	return err
}

func uninstallPersistedWindowsRuntime(ctx context.Context, ownerSID string) error {
	return hostinstall.UninstallPersisted(ctx, ownerSID)
}

func uninstallWindowsRuntime(ctx context.Context, request hostinstall.Request) error {
	return hostinstall.Uninstall(ctx, request)
}
