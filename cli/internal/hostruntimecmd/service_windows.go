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
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
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
			return commitWindowsRuntime(ctx, request)
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

func dispatchElevatedOperation(ctx context.Context, request elevation.Request) (resultErr error) {
	defer func() {
		if runtimeCausesOnly(resultErr, hostinstall.ErrSignedUpdatePending, service.ErrLifecycleUncertain) {
			resultErr = &elevation.RemoteError{Code: "activation_pending", Message: hostinstall.ErrSignedUpdatePending.Error()}
			return
		}
		if runtimeCausesOnly(resultErr, hostinstall.ErrLegacyCarrierRunning) {
			resultErr = &elevation.RemoteError{Code: "legacy_carrier_running", Message: hostinstall.ErrLegacyCarrierRunning.Error()}
		}
	}()
	switch request.Operation {
	case elevation.OperationRuntimeService:
		switch request.Action {
		case elevation.ActionBrowserDomain:
			domainRequest, err := decodeBrowserDomain(bytes.NewReader(request.Payload))
			if err != nil {
				return err
			}
			if !strings.EqualFold(domainRequest.Owner, request.OwnerSID) {
				return errors.New("browser domain owner does not match elevation requester")
			}
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			_, err = machineguard.InstallBrowserDomain(ctx, executable, domainRequest.Owner, domainRequest.Domain)
			return err
		case elevation.ActionUninstallPersist:
			return uninstallPersistedWindowsRuntime(ctx, request.OwnerSID)
		case elevation.ActionPurge:
			return hostinstall.Purge(ctx, request.OwnerSID)
		case elevation.ActionResetPurge:
			return hostinstall.PurgeForReset(ctx, request.OwnerSID)
		case elevation.ActionRepair:
			return repairWindowsInstallation(ctx, request.OwnerSID)
		case elevation.ActionConfigInstall, elevation.ActionConfigRemove:
			unlock, err := lockWindowsNativeInstall(ctx)
			if err != nil {
				return err
			}
			defer func() {
				if err := unlock(); err != nil {
					resultErr = withCleanup("native service operation or its cleanup failed", resultErr, err)
				}
			}()
			definition, err := hostinstall.WindowsConfigServiceDefinition(request.OwnerSID)
			if err != nil {
				return err
			}
			installer, err := service.New(definition)
			if err != nil {
				return err
			}
			if request.Action == elevation.ActionConfigInstall {
				return installer.Install(ctx)
			}
			return installer.Uninstall(ctx)
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
			switch request.Action {
			case elevation.ActionInstall:
				return installWindowsRuntimeFromSuppliedBytes(ctx, installRequest)
			case elevation.ActionCommit:
				return commitWindowsRuntime(ctx, installRequest)
			case elevation.ActionUninstall:
				return uninstallWindowsRuntime(ctx, installRequest)
			case elevation.ActionInstallCommit:
				return installWindowsRuntimeFromSuppliedBytesWithCommit(ctx, installRequest, true)
			}
		}
	case elevation.OperationOpenSSH:
		config, err := elevatedOpenSSHConfig(request.OwnerSID)
		if err != nil {
			return err
		}
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

func elevatedOpenSSHConfig(ownerSID string) (windowsopenssh.Config, error) {
	instance, err := hostinstall.WindowsInstanceForSID(ownerSID)
	if err != nil {
		return windowsopenssh.Config{}, err
	}
	config := windowsopenssh.DefaultConfig(nil)
	config.OwnerSID = ownerSID
	return windowsOpenSSHBootstrapConfig(config, instance), nil
}

func installWindowsRuntimeFromSuppliedBytes(ctx context.Context, request hostinstall.Request) error {
	return installWindowsRuntimeFromSuppliedBytesWithCommit(ctx, request, false)
}

func installWindowsRuntimeFromSuppliedBytesWithCommit(ctx context.Context, request hostinstall.Request, commit bool) (resultErr error) {
	step := "install_source"
	defer func() {
		if resultErr != nil {
			if remote, ok := resultErr.(*elevation.RemoteError); ok && (remote.Code == "maintenance_busy" || remote.Code == "update_verification_failed" || remote.Code == "legacy_carrier_running") {
				return
			}
			resultErr = &windowsInstallationStepError{step: step, cause: resultErr}
		}
	}()
	// A fresh install precedes pairing and has no machine identity yet;
	// hostinstall.Install validates pending requests with their own contract.
	if !request.EnrollmentPending {
		if err := hostinstall.Validate(request, 0); err != nil {
			return err
		}
	}
	if err := request.Source.Verify(request.Executable); err != nil {
		return err
	}
	instance, err := hostinstall.WindowsInstanceForSID(request.OwnerSID)
	if err != nil {
		return err
	}
	unlock, err := lockWindowsNativeInstall(ctx)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = withCleanup("native installation lock cleanup failed", resultErr, unlock())
	}()
	step = "install_recovery"
	previous, loadErr := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if loadErr != nil && !runtimeCausesOnly(loadErr, os.ErrNotExist) {
		return loadErr
	}
	if loadErr == nil && !previous.EnrollmentPending {
		if err := recoverSuppliedWindowsInstallation(ctx, request.OwnerSID, request.UpdateBundle != ""); err != nil {
			return err
		}
		previous, loadErr = hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
		if loadErr != nil {
			return loadErr
		}
	}
	if loadErr == nil {
		layout, err := service.WindowsUserLayout(request.OwnerSID)
		if err != nil {
			return err
		}
		config := windowsUpdatedConfigFor(previous, layout, previous.Source.Version)
		recoverUpdate := updated.RecoverWindowsNativeInstall
		if request.UpdateBundle != "" {
			recoverUpdate = updated.CheckOfflineWindowsNativeInstall
		}
		if err := recoverUpdate(ctx, config); err != nil {
			return fmt.Errorf("recover pending Windows update before supplied install: %w", err)
		}
		// Recovery may commit or restore feature Source and its artifact.
		previous, loadErr = hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
		if loadErr != nil {
			return loadErr
		}
	}
	if request.SuppliedUpdate && request.UpdateBundle == "" {
		request.Source.Distribution = installsource.Custom
		request.Source.AutomaticUpdates = false
	}
	offline := request.UpdateBundle != ""
	if request.UpdateBundle != "" {
		layout, layoutErr := service.WindowsUserLayout(request.OwnerSID)
		if layoutErr != nil {
			return layoutErr
		}
		resolved, cleanup, resolveErr := hostinstall.ResolveUpdateBundle(ctx, request, layout.UpdateStateRoot)
		if resolveErr != nil {
			if resolveErr == hostinstall.ErrUpdateVerificationFailed {
				return &elevation.RemoteError{Code: "update_verification_failed", Message: resolveErr.Error()}
			}
			return resolveErr
		}
		defer cleanup()
		request = resolved
	}
	var snapshot suppliedWindowsInstallation
	if loadErr == nil && !previous.EnrollmentPending {
		snapshot, err = prepareSuppliedWindowsSnapshot(ctx, request.OwnerSID, offline)
		if err != nil {
			return err
		}
		request.RollbackIdentity = snapshot.identity
	}
	nativeReplacement := commit && loadErr == nil && !previous.EnrollmentPending
	replacementStarted := false
	pendingReplacementTakenOver := false
	if loadErr == nil && previous.EnrollmentPending {
		step = "install_pending_daemon"
		resume, err := hostinstall.PreparePendingWindowsDaemonReplacement(ctx, request.OwnerSID)
		if err != nil {
			return err
		}
		defer func() {
			if resultErr != nil && !pendingReplacementTakenOver {
				resultErr = withCleanup("restore pending Windows daemon", resultErr, resume())
			}
		}()
	}
	if !nativeReplacement {
		step = "install_candidate"
		cleanupPin, err := hostinstall.PrepareWindowsReplacement(ctx, request)
		if err != nil {
			return err
		}
		defer func() {
			if !replacementStarted {
				cleanupPin()
			}
		}()
		if loadErr == nil && !previous.EnrollmentPending {
			// Candidate preparation has already proved a descriptor-absent
			// legacy runtime stopped. Only a retained carrier needs live admission.
			_, carrierErr := hostinstall.ReadCarrierConfig(ctx, instance)
			if carrierErr != nil && !runtimeCausesOnly(carrierErr, os.ErrNotExist) {
				return carrierErr
			}
			if carrierErr == nil {
				layout, err := service.WindowsUserLayout(request.OwnerSID)
				if err != nil {
					return err
				}
				abort, err := hostinstall.PrepareSuppliedMaintenance(ctx, layout.HostdSocket, previous.TokenFile, request.Force)
				if err != nil {
					return err
				}
				defer abort()
			}
		}
	}
	// Recovery has its own signed-policy budget; local slot/SCM changes and
	// the final enrollment commit/readiness keep their previous short budget.
	ctx, cancel := context.WithTimeout(ctx, elevation.RuntimeActivationDuration)
	defer cancel()
	if nativeReplacement {
		step = "install_runtime"
		replacementStarted = true
		err := hostinstall.ReplaceWindowsInstallation(ctx, request, snapshot, func(ctx context.Context) error {
			if err := hostinstall.StopWindowsReplacement(ctx, request.OwnerSID); err != nil {
				return err
			}
			prepare := updated.PrepareWindowsNativeInstall
			step = "install_updater"
			if offline {
				prepare = updated.PrepareOfflineWindowsNativeInstall
			}
			rollbackIdentity, restoreJournal, err := prepare(ctx, request.OwnerSID)
			if err != nil {
				return fmt.Errorf("prepare Windows updater for supplied install: %w", err)
			}
			request.RollbackIdentity = rollbackIdentity
			step = "install_runtime"
			if err := hostinstall.InstallWindowsReplacement(ctx, request); err != nil {
				return errors.Join(fmt.Errorf("install supplied Windows runtime: %w", err), restoreJournal())
			}
			step = "install_commit"
			if err := hostinstall.Commit(request); err != nil {
				return errors.Join(err, restoreJournal())
			}
			step = "install_readiness"
			if err := hostinstall.EnsureCommittedWindowsHostdReady(ctx, request); err != nil {
				return errors.Join(fmt.Errorf("Paperboat host service did not become ready after installation: %w", err), restoreJournal())
			}
			return nil
		})
		if errors.Is(err, hostdproto.ErrMaintenanceBusy) {
			return &elevation.RemoteError{Code: "maintenance_busy", Message: "Paperboat has protected work; no services were stopped"}
		}
		return err
	}
	restoreServices := func() error {
		recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(ctx), elevation.RuntimeActivationDuration)
		defer recoveryCancel()
		if loadErr != nil {
			return nil
		}
		if previous.EnrollmentPending {
			// The verified pending declaration has its own deferred restoration.
			return nil
		}
		return hostinstall.Repair(recoveryCtx, previous.OwnerSID)
	}
	replacementStarted = true
	step = "install_runtime"
	// Pending installs own only the local daemon. The pending installer stops
	// that daemon before replacing bytes; enrolled recovery needs roles that do
	// not exist until pairing has completed.
	if loadErr == nil && !previous.EnrollmentPending {
		if err := hostinstall.Stop(ctx, request.OwnerSID); err != nil {
			return err
		}
	}
	step = "install_updater"
	rollbackIdentity, restoreJournal, err := updated.PrepareWindowsNativeInstall(ctx, request.OwnerSID)
	if err != nil {
		return errors.Join(fmt.Errorf("prepare Windows updater for supplied install: %w", err), restoreServices())
	}
	request.RollbackIdentity = rollbackIdentity
	step = "install_runtime"
	if err := hostinstall.Install(ctx, request); err != nil {
		return errors.Join(fmt.Errorf("install supplied Windows runtime: %w", err), restoreJournal(), restoreServices())
	}
	pendingReplacementTakenOver = true
	if commit {
		step = "install_commit"
		if err := hostinstall.Commit(request); err != nil {
			// Commit may publish durable installation state before a cleanup or
			// security probe fails. Preserve that state for explicit recovery.
			return &windowsInstallCommitFailure{cause: err}
		}
		// Durable commit precedes the exact enrolled owner's readiness probe.
		step = "install_readiness"
		if err := hostinstall.EnsureCommittedWindowsHostdReady(ctx, request); err != nil {
			return fmt.Errorf("Paperboat host service did not become ready after installation: %w", err)
		}
	}
	return nil
}

type windowsInstallationStepError struct {
	step  string
	cause error
}

func (e *windowsInstallationStepError) Error() string {
	return elevation.RuntimeInstallFailureMessage(e.step)
}
func (e *windowsInstallationStepError) Unwrap() error                { return e.cause }
func (e *windowsInstallationStepError) ElevationFailureCode() string { return e.step }

type windowsInstallCommitFailure struct {
	cause error
}

func (*windowsInstallCommitFailure) Error() string {
	return "Paperboat installation commit or cleanup did not finish; installation may already be committed. Run pb status and pb doctor before retrying."
}

func (e *windowsInstallCommitFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func commitWindowsRuntime(ctx context.Context, request hostinstall.Request) (resultErr error) {
	unlock, err := lockWindowsNativeInstall(ctx)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = withCleanup("native installation lock cleanup failed", resultErr, unlock())
	}()
	return hostinstall.Commit(request)
}

func lockWindowsNativeInstall(ctx context.Context) (func() error, error) {
	if ctx == nil {
		return nil, errors.New("nil Windows native install context")
	}
	runtime.LockOSThread()
	acquired := false
	defer func() {
		if !acquired {
			runtime.UnlockOSThread()
		}
	}()
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
			return nil, runtimeFailure("waiting for the native installation lock failed", err, windows.CloseHandle(handle))
		}
		state, waitErr := windows.WaitForSingleObject(handle, uint32((100 * time.Millisecond).Milliseconds()))
		if waitErr != nil {
			return nil, runtimeFailure("waiting for the native installation lock failed", waitErr, windows.CloseHandle(handle))
		}
		if state == windows.WAIT_OBJECT_0 || state == windows.WAIT_ABANDONED {
			acquired = true
			return func() error {
				defer runtime.UnlockOSThread()
				return runtimeFailureIfAny("releasing the native installation lock failed", windows.ReleaseMutex(handle), windows.CloseHandle(handle))
			}, nil
		}
		if state != uint32(windows.WAIT_TIMEOUT) {
			return nil, runtimeFailure("waiting for the native installation lock failed", windows.CloseHandle(handle))
		}
	}
}

func repairWindowsInstallation(ctx context.Context, ownerSID string) (resultErr error) {
	unlock, err := lockWindowsNativeInstall(ctx)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = withCleanup("native installation lock cleanup failed", resultErr, unlock())
	}()
	if err := recoverSuppliedWindowsInstallation(ctx, ownerSID, false); err != nil {
		return err
	}
	identity, _, inspectErr := updated.InspectWindowsNativeInstallation(ctx, ownerSID, true)
	if inspectErr == nil {
		err = hostinstall.RepairWindowsIdentity(ctx, ownerSID, identity)
	} else if inspectErr == updated.ErrNativeInstalledSourceMismatch {
		// A damaged fixed slot has no derived identity yet; native Repair can
		// restore its exact protected Source from the owned immutable pin.
		err = hostinstall.Repair(ctx, ownerSID)
	} else {
		return inspectErr
	}
	if runtimeCausesOnly(err, hostinstall.ErrNotInstalled) {
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
