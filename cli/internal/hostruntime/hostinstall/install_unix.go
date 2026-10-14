//go:build darwin || linux

package hostinstall

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

const SchemaV1 = "paperboat.host-install/v1"

const journalSchemaV1 = "paperboat.host-install-journal/v1"

var (
	ErrInvalidRequest = errors.New("invalid privileged installation request")
	ErrNotPrivileged  = errors.New("privileged installation requires administrator approval")
)

// Request is the complete allowlist accepted by the privileged installer.
// It deliberately has no generic command, argument, path, or environment fields.
type Request struct {
	UpdateBundle           string                   `json:"update_bundle,omitempty"`
	Force                  bool                     `json:"force,omitempty"`
	SuppliedUpdate         bool                     `json:"supplied_update,omitempty"`
	Schema                 string                   `json:"schema"`
	Platform               string                   `json:"platform"`
	User                   string                   `json:"user"`
	UID                    int                      `json:"uid"`
	Group                  string                   `json:"group"`
	GID                    int                      `json:"gid"`
	Executable             string                   `json:"executable"`
	Artifact               bootstrap.ArtifactTarget `json:"artifact"`
	Source                 installsource.Source     `json:"source"`
	SessionOwnerGeneration string                   `json:"session_owner_generation,omitempty"`
	Home                   string                   `json:"home"`
	Path                   string                   `json:"path"`
	StateRoot              string                   `json:"state_root"`
	WorkspaceRoot          string                   `json:"workspace_root"`
	ControlURL             string                   `json:"control_url"`
	UserMachineID          string                   `json:"machine_id"`
	Shell                  string                   `json:"shell"`
	HelperListenAddress    string                   `json:"helper_listen_address"`
	EnrollmentPending      bool                     `json:"enrollment_pending"`
	InstallationGeneration int64                    `json:"installation_generation"`
}

func Decode(reader io.Reader) (Request, error) {
	var request Request
	if err := decodeInstallJSON(reader, &request, 128<<10); err != nil {
		return Request{}, err
	}
	return request, nil
}

func Install(ctx context.Context, request Request) error { return install(ctx, request, false) }
func install(ctx context.Context, request Request, nativeLockHeld bool) error {
	return installWithMaintenance(ctx, request, nativeLockHeld, nil)
}

func installWithMaintenance(ctx context.Context, request Request, nativeLockHeld bool, suppliedForce *bool) (resultErr error) {
	intent := unixEnrollmentInstall
	if suppliedForce != nil {
		intent = unixSuppliedInstall
	}
	return installWithIntent(ctx, request, nativeLockHeld, intent, suppliedForce, PreparedUpdateHooks{})
}

func installWithIntent(ctx context.Context, request Request, nativeLockHeld bool, intent unixInstallIntent, suppliedForce *bool, hooks PreparedUpdateHooks) (resultErr error) {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	owner := invokingUID()
	if intent == unixPreparedUpdate {
		owner = request.UID
	}
	if err := Validate(request, owner); err != nil {
		return err
	}
	paths := platformPaths(request.UID)
	var err error
	var legacyHost *service.Installer
	// hostd now owns the runtime control plane. The separate privileged host
	// service is retained only for power policy and must not bind the same
	// loopback address as hostd.
	if runtime.GOOS != "darwin" {
		legacyHost, err = hostInstallerWithMissing(request, paths, true)
		if err != nil {
			return errors.Join(ErrInvalidRequest, err)
		}
	}
	hostd, updater, err := installersWithMaintenance(request, paths, true, intent != unixEnrollmentInstall)
	if err != nil {
		return errors.Join(ErrInvalidRequest, err)
	}
	lifecycle, err := newLifecycleManagerWithRestorePolicy(request, paths, legacyHost, hostd, updater, false, hooks.AuthorizeRestore)
	if err != nil {
		return errors.Join(ErrInvalidRequest, err)
	}
	if !nativeLockHeld {
		activationLock, lockErr := lockUnixInstallState(ctx, paths.updateState)
		if lockErr != nil {
			return lockErr
		}
		if activationLock != nil {
			defer func() {
				resultErr = withCleanup("installation cleanup failed", resultErr, activationLock.Close())
			}()
		}
	}
	if intent != unixPreparedUpdate {
		if err := prepareUnixNativeRecovery(ctx, paths, request.UID, intent == unixSuppliedInstall); err != nil {
			return err
		}
	}
	// Only protected activation-lock infrastructure was created above. A corrupt or stale
	// service journal must stop the install before token, directory, binary, or
	// native declaration changes begin.
	if err := lifecycle.Recover(ctx); err != nil {
		return err
	}

	coldCarrier, err := preflightUnixCarrier(ctx, request, paths)
	if err != nil {
		return err
	}
	// Cold admission proves every legacy peer-owning native role stopped.
	// Only that native proof permits installing without a live hostd socket.
	// Existing independent carriers always require workload admission and drain.
	if suppliedForce != nil && !coldCarrier {
		timeout := 35 * time.Second
		if intent == unixPreparedUpdate {
			timeout = hooks.DrainTimeout + 5*time.Second
		}
		abort, err := prepareMaintenance(ctx, paths.hostdSocket, paths.hostdToken, *suppliedForce, timeout)
		if err != nil {
			return err
		}
		defer func() {
			cleanupErr := abort()
			if intent == unixPreparedUpdate {
				resultErr = withCleanup("prepared update maintenance cleanup failed", resultErr, cleanupErr)
			}
		}()
	}
	// A pre-lifecycle binary transaction may still describe the fixed slot.
	// Recover its exact protected baseline before capturing the new native
	// transaction, after cold admission has proved legacy roles stopped.
	if intent != unixPreparedUpdate {
		if _, err := recoverInterrupted(paths); err != nil {
			return err
		}
	}
	if intent == unixPreparedUpdate {
		if err := hooks.Prepare(ctx); err != nil {
			return err
		}
	}

	resultErr = lifecycle.Replace(ctx, func(ctx context.Context) error {
		if err := publishUnixControlLifetime(ctx, paths, hostd, coldCarrier); err != nil {
			return err
		}
		if err := stopUnixRuntimeRoles(ctx, legacyHost, hostd, updater); err != nil {
			return err
		}
		// Disable declarations that could reboot into an embedded native peer
		// before a separately owned carrier is admitted.
		if coldCarrier {
			component, err := hostd.NativeComponent(nil)
			if err != nil {
				return err
			}
			if err := component.Stop(ctx); err != nil {
				return err
			}
			if err := component.Uninstall(ctx); err != nil {
				return err
			}
		}
		if intent != unixPreparedUpdate {
			if err := installMachineGuard(ctx, request); err != nil {
				return err
			}
		}
		if err := ensureHostdToken(paths, request); err != nil {
			return err
		}
		if err := ensureManagedDirectories(paths, request); err != nil {
			return err
		}
		if _, err := ensureEnvironmentHostCredential(ctx, paths, request); err != nil {
			return err
		}
		var journal installJournal
		if intent != unixPreparedUpdate {
			journal, err = newInstallJournal(paths, request.Source)
			if err != nil {
				return err
			}
			if err := writeJournal(paths.journal, journal); err != nil {
				return err
			}
			if err := updated.PrepareUnixNativeInstall(paths.updateState); err != nil {
				return errors.Join(err, removeJournal(paths.journal))
			}
		}
		if err := stageBinary(request.Executable, paths.workerNext, request.Source); err != nil {
			if intent == unixPreparedUpdate {
				return err
			}
			return errors.Join(err, updated.RollbackUnixNativeInstall(paths.updateState), removeJournal(paths.journal))
		}
		if intent == unixPreparedUpdate {
			// The native lifecycle has captured and verified the prior source pin;
			// it owns restoration, so no competing binary rollback journal is made.
			if err := os.Rename(paths.workerNext, paths.worker); err != nil {
				return err
			}
		} else if err := activateBinary(paths.worker, paths.workerNext, paths.workerRollback); err != nil {
			return err
		}
		if intent != unixPreparedUpdate {
			journal.Stage, journal.UpdatedAt = "binaries_activated", time.Now().UTC()
			if err := writeJournal(paths.journal, journal); err != nil {
				return err
			}
		}
		// Migrate away from the pre-hostd monolithic worker service. Leaving it
		// active would start a second runtime against the same control endpoint.
		legacyWorker, legacyErr := legacyWorkerInstaller(request, paths)
		if legacyErr != nil {
			return legacyErr
		}
		_, legacyWorkerWasInstalledErr := os.Lstat(legacyWorker.DefinitionPath())
		if legacyWorkerWasInstalledErr != nil && !installNotExistOnly(legacyWorkerWasInstalledErr) {
			return legacyWorkerWasInstalledErr
		}
		if err := legacyWorker.Uninstall(ctx); err != nil {
			return err
		}
		// Early macOS host builds used the privileged host-service label for the
		// monolithic runtime. The split hostd binds the runtime health/control port,
		// while the privileged role owns only power policy. Remove the obsolete
		// declaration before starting hostd so both cannot contend for the port.
		var obsoleteDarwinHost *service.Installer
		if runtime.GOOS == "darwin" {
			obsoleteDarwinHost, err = hostInstallerWithMissing(request, paths, true)
			if err != nil {
				return err
			}
			if err := obsoleteDarwinHost.Uninstall(ctx); err != nil {
				return err
			}
		}
		if err := prepareSessionOwner(ctx, request, paths); err != nil {
			return err
		}
		if coldCarrier {
			if err := prepareColdUnixCarrier(ctx, request, paths); err != nil {
				return err
			}
		}
		if err := applyUnixNativeRoles(ctx, legacyHost, hostd, updater, request.HelperListenAddress, false); err != nil {
			return err
		}

		if intent == unixPreparedUpdate {
			return commitValidatedPreparedUpdate(ctx, request, paths, hooks.Validate)
		}
		journal.Stage, journal.UpdatedAt = "services_started", time.Now().UTC()
		if err := writeJournal(paths.journal, journal); err != nil {
			return err
		}
		return commitPreparedInstallationLocked(ctx, request, paths, true)
	})
	if resultErr == nil && intent == unixSuppliedInstall {
		return finalizePreparedUpdateLocked(ctx, request.UID)
	}
	return resultErr
}

func Commit(ctx context.Context, request Request) error {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if err := Validate(request, invokingUID()); err != nil {
		return err
	}
	return commitPreparedInstallation(ctx, request, platformPaths(request.UID))
}

func commitPreparedInstallation(ctx context.Context, request Request, paths installPaths) error {
	return commitPreparedInstallationLocked(ctx, request, paths, false)
}
func commitPreparedInstallationLocked(ctx context.Context, request Request, paths installPaths, nativeLockHeld bool) (resultErr error) {
	journal, err := loadJournal(paths.journal)
	if installNotExistOnly(err) {
		installed, err := loadInstallMetadata(paths.metadata, request.UID)
		if err != nil {
			return err
		}
		if installed.Source != request.Source || installed.SessionOwnerGeneration != request.Source.SHA256[:32] {
			return ErrInvalidRequest
		}
		return installed.Source.Verify(paths.worker)
	}
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if journal.Stage != "services_started" || journal.Source != request.Source {
		return ErrInvalidRequest
	}
	if _, statErr := os.Lstat(paths.updateState); statErr == nil {
		if !nativeLockHeld {
			lock, lockErr := updated.LockUnixNativeInstall(ctx, paths.updateState)
			if lockErr != nil {
				return lockErr
			}
			defer func() { resultErr = withCleanup("installation cleanup failed", resultErr, lock.Close()) }()
		}
	} else if !installNotExistOnly(statErr) {
		return statErr
	}
	for _, pair := range [][2]string{{paths.workerRollback, paths.workerPrevious}} {
		if err := replacePrevious(pair[0], pair[1]); err != nil {
			return err
		}
	}
	request.SessionOwnerGeneration = request.Source.SHA256[:32]
	if err := writeInstallMetadata(paths.metadata, request); err != nil {
		return err
	}
	if err := removeJournal(paths.journal); err != nil {
		return errors.Join(err, writeJournal(paths.journal, journal))
	}
	if err := updated.CommitUnixNativeInstall(paths.updateState); err != nil {
		return errors.Join(err, writeJournal(paths.journal, journal))
	}
	return removeInstallFile(paths.workerRollback)
}

func Uninstall(ctx context.Context, request Request) error {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if err := Validate(request, invokingUID()); err != nil {
		return err
	}
	return uninstallValidated(ctx, request, platformPaths(request.UID))
}

func UninstallPersisted(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	uid := invokingUID()
	paths := platformPaths(uid)
	request, err := loadInstallMetadata(paths.metadata, uid)
	if err != nil {
		return err
	}
	return uninstallValidated(ctx, request, paths)
}

// ResetCLIForCurrentAccount proves the fixed account CLI before destructive reset.
func ResetCLIForCurrentAccount() (string, error) {
	if os.Geteuid() != 0 {
		return "", ErrNotPrivileged
	}
	paths := platformPaths(invokingUID())
	if _, err := os.Lstat(paths.worker); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	for path := paths.worker; path != string(filepath.Separator); path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || ownerUID(info) != 0 || info.Mode().Perm()&0022 != 0 {
			return "", ErrInvalidRequest
		}
		if path == paths.worker && !info.Mode().IsRegular() {
			return "", ErrInvalidRequest
		}
	}
	request, err := loadInstallMetadata(paths.metadata, invokingUID())
	if installNotExistOnly(err) {
		// Reset removed metadata but retained the protected canonical CLI.
		_, err = installsource.Inspect(paths.worker, "reset", installsource.Custom)
		if err != nil {
			return "", err
		}
		return paths.worker, nil
	}
	if err != nil {
		return "", err
	}
	if err := request.Source.Verify(paths.worker); err != nil {
		return "", err
	}
	return paths.worker, nil
}

type resetCLIContextKey struct{}

// UninstallPersistedPreservingCLI keeps only the source-verified canonical CLI.
func UninstallPersistedPreservingCLI(ctx context.Context) error {
	keep, err := ResetCLIForCurrentAccount()
	if err != nil {
		return err
	}
	return UninstallPersisted(context.WithValue(ctx, resetCLIContextKey{}, keep))
}

// Repair re-applies both native role declarations as one transaction. The
// lifecycle journal is recovered before any declaration or service-manager
// mutation, so a stale/corrupt prior operation fails closed.
func Repair(ctx context.Context, request Request) (resultErr error) {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if err := Validate(request, invokingUID()); err != nil {
		return err
	}
	paths := platformPaths(request.UID)
	persisted, err := loadInstallMetadata(paths.metadata, request.UID)
	if err != nil {
		return err
	}
	if persisted.Source.SHA256 != request.Source.SHA256 || persisted.Source.Length != request.Source.Length || persisted.Source.Version != request.Source.Version {
		return ErrInvalidRequest
	}
	activationLock, err := lockUnixInstallState(ctx, paths.updateState)
	if err != nil {
		return err
	}
	if activationLock != nil {
		defer func() { resultErr = withCleanup("repair cleanup failed", resultErr, activationLock.Close()) }()
	}
	if err = prepareUnixNativeRecovery(ctx, paths, request.UID, false); err != nil {
		return err
	}
	persisted, err = loadInstallMetadata(paths.metadata, request.UID)
	if err != nil {
		return err
	}
	if persisted.Source != request.Source {
		return ErrInvalidRequest
	}
	request = persisted

	if err := ensureSharedUserParents(paths); err != nil {
		return err
	}
	hostd, updater, err := pendingInstallers(request, paths)
	if err != nil {
		return errors.Join(ErrInvalidRequest, err)
	}
	var legacyHost *service.Installer
	if runtime.GOOS != "darwin" {
		legacyHost, err = hostInstallerWithMissing(request, paths, true)
		if err != nil {
			return errors.Join(ErrInvalidRequest, err)
		}
	}
	lifecycle, err := newLifecycleManager(request, paths, legacyHost, hostd, updater)
	if err != nil {
		return errors.Join(ErrInvalidRequest, err)
	}
	if err := lifecycle.Recover(ctx); err != nil {
		return err
	}

	coldCarrier, err := preflightUnixCarrier(ctx, request, paths)
	if err != nil {
		return err
	}
	return lifecycle.Replace(ctx, func(ctx context.Context) error {
		if err := publishUnixControlLifetime(ctx, paths, hostd, coldCarrier); err != nil {
			return err
		}
		if coldCarrier {
			component, err := hostd.NativeComponent(nil)
			if err != nil {
				return err
			}
			if err := component.Stop(ctx); err != nil {
				return err
			}
			if err := component.Uninstall(ctx); err != nil {
				return err
			}
		}
		if err := ensureHostdToken(paths, request); err != nil {
			return err
		}
		if err := ensureManagedDirectories(paths, request); err != nil {
			return err
		}
		credentialCreated, err := ensureEnvironmentHostCredential(ctx, paths, request)
		if err != nil {
			return err
		}
		if _, err := checkRegularFile(paths.worker); err != nil {
			return hostInstallFailure("installed runtime binary could not be validated", ErrInvalidRequest, err)
		}
		if request.SessionOwnerGeneration != "" {
			if _, err := SessionOwnerRecorded(ctx, request); err != nil {
				return err
			}
		} else {
			controller, err := service.ComponentController(request.Platform, service.HostdKind, request.UID, service.ExecRunner{})
			if err != nil {
				return err
			}
			native, ok := controller.(service.NativeLifecycleController)
			if !ok {
				return service.ErrInvalidDefinition
			}
			status, err := native.Inspect(ctx, hostd.DefinitionPath())
			if err != nil {
				return err
			}
			if status.Running {
				abort, err := PrepareSuppliedMaintenance(ctx, paths.hostdSocket, paths.hostdToken, false)
				if err != nil {
					return err
				}
				defer abort()
			}
			if err := stopUnixRuntimeRoles(ctx, legacyHost, hostd, updater); err != nil {
				return err
			}
		}
		if credentialCreated && request.SessionOwnerGeneration != "" {
			// A running systemd service cannot gain a newly declared credential
			// without a process restart. Repair is the explicit user-authorized
			// maintenance path for this one-time cutover.
			if err := stopUnixRuntimeRoles(ctx, legacyHost, hostd, updater); err != nil {
				return err
			}
		}
		request.Executable = paths.worker
		if err := PrepareSessionOwner(ctx, request); err != nil {
			return err
		}
		if coldCarrier {
			if err := prepareColdUnixCarrier(ctx, request, paths); err != nil {
				return err
			}
		}
		if err := applyUnixNativeRoles(ctx, legacyHost, hostd, updater, request.HelperListenAddress, false); err != nil {
			return err
		}
		request.SessionOwnerGeneration = request.Source.SHA256[:32]
		return writeInstallMetadata(paths.metadata, request)
	})
}

// RepairPersisted restores the exact previously installed host role without
// accepting caller-selected paths, commands, identities, or service names.
// It is the safe recovery path used by `pb tunnel create` when the durable
// host runtime is enrolled but stopped or damaged.
func RepairPersisted(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	uid := invokingUID()
	paths := platformPaths(uid)
	request, err := loadInstallMetadata(paths.metadata, uid)
	if err != nil {
		return err
	}
	return Repair(ctx, request)
}

// Stop stops hostd and updated in dependency order without removing their
// declarations. Recovery is always performed first so an interrupted prior
// install/repair cannot be hidden by a new stop request.
func Stop(ctx context.Context, request Request) (resultErr error) {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if err := Validate(request, invokingUID()); err != nil {
		return err
	}
	paths := platformPaths(request.UID)
	activationLock, err := lockUnixInstallState(ctx, paths.updateState)
	if err != nil {
		return err
	}
	if activationLock != nil {
		defer func() { resultErr = withCleanup("stop cleanup failed", resultErr, activationLock.Close()) }()
	}
	if err = prepareUnixNativeRecovery(ctx, paths, request.UID, false); err != nil {
		return err
	}

	hostd, updater, err := pendingInstallers(request, paths)
	if err != nil {
		return errors.Join(ErrInvalidRequest, err)
	}
	var legacyHost *service.Installer
	if runtime.GOOS != "darwin" {
		legacyHost, err = hostInstallerWithMissing(request, paths, true)
		if err != nil {
			return errors.Join(ErrInvalidRequest, err)
		}
	}
	lifecycle, err := newLifecycleManager(request, paths, legacyHost, hostd, updater)
	if err != nil {
		return errors.Join(ErrInvalidRequest, err)
	}
	if err := lifecycle.Recover(ctx); err != nil {
		return err
	}
	return lifecycle.Replace(ctx, func(ctx context.Context) error { return stopUnixRuntimeRoles(ctx, legacyHost, hostd, updater) })
}

func uninstallValidated(ctx context.Context, request Request, paths installPaths) (resultErr error) {
	if request.EnrollmentPending {
		// Before pairing there are only supplied bytes and installation state;
		// enrolled role definitions require an identity and health endpoint that
		// do not exist yet. The caller's inventory sweep handles any stale roles.
		if err := ctx.Err(); err != nil {
			return err
		}
		return withUninstallStage("uninstall_files", removeInstalledFiles(ctx, paths))
	}
	hostd, updater, err := pendingInstallers(request, paths)
	if err != nil {
		return withUninstallStage("uninstall_prepare", errors.Join(ErrInvalidRequest, err))
	}
	var legacyHost *service.Installer
	if runtime.GOOS != "darwin" {
		legacyHost, err = hostInstallerWithMissing(request, paths, true)
		if err != nil {
			return withUninstallStage("uninstall_prepare", errors.Join(ErrInvalidRequest, err))
		}
	}
	lifecycle, err := newLifecycleManager(request, paths, legacyHost, hostd, updater)
	if err != nil {
		return withUninstallStage("uninstall_prepare", errors.Join(ErrInvalidRequest, err))
	}
	activationLock, err := updated.LockUnixActivationForUninstall(paths.updateState)
	if err != nil {
		return withUninstallStage("uninstall_lock", err)
	}
	if activationLock != nil {
		defer func() {
			resultErr = withCleanup("installation cleanup failed", resultErr, withUninstallStage("uninstall_lock_close", activationLock.Close()))
		}()
	}

	if err := prepareUnixNativeRecovery(ctx, paths, request.UID, false); err != nil {
		return withUninstallStage("uninstall_recover", err)
	}
	// Recovery is intentionally before the first stop/remove operation. A
	// malformed or stale journal therefore leaves native declarations intact
	// for an explicit repair decision.
	if err := lifecycle.Recover(ctx); err != nil {
		return withUninstallStage("uninstall_recover", err)
	}
	restorePower := func(context.Context) error { return nil }
	{
		restorePower = func(restoreCtx context.Context) error {
			return hostservice.NewPlatformApplier(filepath.Join(paths.runtimeState, "power-baseline.json")).Apply(restoreCtx, hostservice.AllowSleep)
		}
	}
	return finalizeUninstall(ctx, request, paths, func(ctx context.Context) error {
		if err := RetireSessionOwners(ctx, request, true); err != nil {
			return withUninstallStage("uninstall_sessions", err)
		}
		controller := service.UnixUpdateActivator{Platform: request.Platform, UID: request.UID, Runner: service.ExecRunner{}}
		return uninstallWithActivation(ctx, controller.RemoveForUninstall, lifecycle.Uninstall)
	}, restorePower)
}

// finalizeUninstall is the only path allowed to remove installed binaries and
// metadata. Native removal and host power restoration must both succeed first;
// otherwise an active or partially restored service could be left pointing at
// files that have already been deleted.
func finalizeUninstall(ctx context.Context, request Request, paths installPaths, uninstallService func(context.Context) error, restorePower func(context.Context) error) error {
	if ctx == nil || uninstallService == nil || restorePower == nil {
		return ErrInvalidRequest
	}
	if err := uninstallService(ctx); err != nil {
		return withUninstallStage("uninstall_services", err)
	}
	if err := restorePower(ctx); err != nil {
		return withUninstallStage("uninstall_power", err)
	}
	journal, journalErr := loadJournal(paths.journal)
	if journalErr == nil {
		return withUninstallStage("uninstall_files", rollbackFiles(paths, journal))
	}
	if !installNotExistOnly(journalErr) {
		return withUninstallStage("uninstall_journal", journalErr)
	}
	return withUninstallStage("uninstall_files", removeInstalledFiles(ctx, paths))
}

func installers(request Request, paths installPaths) (*service.Installer, *service.Installer, error) {
	return installersWithMissing(request, paths, false)
}

func pendingInstallers(request Request, paths installPaths) (*service.Installer, *service.Installer, error) {
	return installersWithMissing(request, paths, true)
}

func installersWithMissing(request Request, paths installPaths, allowMissingExecutable bool) (*service.Installer, *service.Installer, error) {
	return installersWithMaintenance(request, paths, allowMissingExecutable, false)
}

func installersWithMaintenance(request Request, paths installPaths, allowMissingExecutable, maintenancePending bool) (*service.Installer, *service.Installer, error) {
	layout, err := componentLayout(paths)
	if err != nil {
		return nil, nil, err
	}
	hostdController, err := service.ComponentController(request.Platform, service.HostdKind, request.UID, service.ExecRunner{})
	if err != nil {
		return nil, nil, err
	}
	hostdConfig := service.ComponentConfig{
		Layout: layout, User: request.User, Group: request.Group, UID: request.UID, GID: request.GID,
		HostdTokenFile: paths.hostdToken, Environment: workerEnvironment(request), Controller: hostdController,
	}
	if request.Source.Validate() == nil {
		sourceBody, err := json.Marshal(request.Source)
		if err != nil {
			return nil, nil, err
		}
		hostdConfig.Environment["PAPERBOAT_INSTALL_SOURCE"] = base64.RawStdEncoding.EncodeToString(sourceBody)
	}
	if maintenancePending {
		hostdConfig.Environment["PAPERBOAT_NATIVE_UPDATE_PENDING"] = "true"
	}
	if request.Platform == "linux" {
		hostdConfig.EncryptedCredentials = map[string]string{environmentkey.CredentialName: paths.environmentCredential}
	}
	var hostd *service.Installer
	if allowMissingExecutable {
		hostd, err = service.NewHostdInstallerPending(hostdConfig)
	} else {
		hostd, err = service.NewHostdInstaller(hostdConfig)
	}
	if err != nil {
		return nil, nil, err
	}
	updaterController, err := service.ComponentController(request.Platform, service.UpdaterKind, request.UID, service.ExecRunner{})
	if err != nil {
		return nil, nil, err
	}
	updaterConfig := service.ComponentConfig{
		Layout: layout, User: request.User, Group: request.Group, UID: request.UID, GID: request.GID,
		HostdTokenFile: paths.hostdToken, ReleaseRepository: request.Artifact.RepositoryURL, MachineID: request.UserMachineID, Source: request.Source,
		HealthURL: "http://" + request.HelperListenAddress + "/healthz", Controller: updaterController,
	}
	var updater *service.Installer
	if allowMissingExecutable {
		updater, err = service.NewUpdaterInstallerPending(updaterConfig)
	} else {
		updater, err = service.NewUpdaterInstaller(updaterConfig)
	}
	if err != nil {
		return nil, nil, err
	}
	return hostd, updater, nil
}

func newLifecycleManager(request Request, paths installPaths, host, hostd, updater *service.Installer) (*service.LifecycleManager, error) {
	return newLifecycleManagerWithCarrierScope(request, paths, host, hostd, updater, false)
}
func newLifecycleManagerWithCarrierScope(request Request, paths installPaths, host, hostd, updater *service.Installer, carrierOnly bool) (*service.LifecycleManager, error) {
	return newLifecycleManagerWithRestorePolicy(request, paths, host, hostd, updater, carrierOnly, nil)
}

func newLifecycleManagerWithRestorePolicy(request Request, paths installPaths, host, hostd, updater *service.Installer, carrierOnly bool, authorize func(context.Context, installsource.Source, string) error) (*service.LifecycleManager, error) {
	if hostd == nil || updater == nil {
		return nil, service.ErrLifecycleInvalid
	}
	probe, err := service.NewHTTPReadinessProbe("http://" + request.HelperListenAddress + "/healthz")
	if err != nil {
		return nil, err
	}
	carrierConfig, err := deriveUnixCarrierConfig(request, paths)
	if err != nil {
		return nil, err
	}
	retained, readErr := readCarrierConfigFile(context.Background(), paths.instance)
	if readErr == nil {
		carrierConfig = retained
	} else if !installNotExistOnly(readErr) {
		return nil, readErr
	}
	carrier, err := carrierInstallerPending(carrierConfig)
	if err != nil {
		return nil, err
	}
	var installation service.NativeInstallationState = unixCarrierInstallation{request: request, paths: paths, carrierOnly: carrierOnly}
	if authorize != nil {
		installation = preparedUnixInstallation{state: installation, paths: paths, authorize: authorize}
	}
	return service.NewHostLifecycleManager(service.HostLifecycleConfig{
		Carrier: carrier,
		CarrierProbe: func(ctx context.Context) error {
			current, err := readCarrierConfigFile(ctx, paths.instance)
			if err != nil {
				return err
			}
			return waitCarrierReady(ctx, current)
		},
		NativeInstallation: installation,
		StateRoot:          filepath.Join(paths.root, "service-lifecycle"),
		Host:               host,
		Hostd:              hostd,
		Updater:            updater,
		HostdProbe:         probe,
		// updated has no public HTTP health endpoint. Its native ready state and
		// authenticated hostd/update control transaction are the readiness
		// boundary; do not invent a second loopback endpoint.
		UpdaterProbe: nil,
	})
}

func legacyWorkerInstaller(request Request, paths installPaths) (*service.Installer, error) {
	controller := service.Controller(service.SystemdController{Runner: service.ExecRunner{}})
	if runtime.GOOS == "darwin" {
		controller = service.LaunchdController{Runner: service.ExecRunner{}, UID: request.UID}
	}
	return service.NewPending(service.Config{
		Platform: request.Platform, Kind: service.WorkerKind, ConfigRoot: string(os.PathSeparator), Executable: paths.worker,
		User: request.User, Group: request.Group, Arguments: []string{"daemon", "__runtime-host"}, Controller: controller,
		Environment: workerEnvironment(request),
	})
}

func componentLayout(paths installPaths) (service.Layout, error) {
	layout, err := service.DefaultLayout(runtime.GOOS)
	if err != nil {
		return service.Layout{}, err
	}
	layout.Instance = paths.instance
	layout.InstallRoot = paths.root
	layout.ReleasesRoot = filepath.Join(paths.root, "releases")
	layout.Binary = paths.worker
	layout.BinaryRollback = paths.workerRollback
	layout.BinaryStaged = paths.workerNext
	layout.UpdateStateRoot = paths.updateState
	layout.HostdSocket = paths.hostdSocket
	if err := layout.Validate(); err != nil {
		return service.Layout{}, err
	}
	return layout, nil
}

func hostInstaller(request Request, paths installPaths) (*service.Installer, error) {
	return hostInstallerWithMissing(request, paths, false)
}

func hostInstallerWithMissing(request Request, paths installPaths, allowMissingExecutable bool) (*service.Installer, error) {
	instance := "u" + strconv.Itoa(request.UID)
	hostController := service.Controller(service.SystemdController{Runner: service.ExecRunner{}, Unit: "paperboat-runtime-privileged-" + instance + ".service"})
	rootGroup := "root"
	if runtime.GOOS == "darwin" {
		rootGroup = "wheel"
		hostController = service.LaunchdController{Runner: service.ExecRunner{}, UID: request.UID, Label: service.HostLabel + "." + instance}
	}
	config := service.Config{
		Platform: request.Platform, Kind: service.HostKind, Instance: instance, ConfigRoot: string(os.PathSeparator), Executable: paths.worker,
		User: "root", Group: rootGroup, Arguments: []string{
			"daemon", "__runtime-host-service", "--uid", strconv.Itoa(request.UID), "--gid", strconv.Itoa(request.GID),
			"--listen-address", request.HelperListenAddress,
		}, Controller: hostController,
	}
	var host *service.Installer
	var err error
	if allowMissingExecutable {
		host, err = service.NewPending(config)
	} else {
		host, err = service.New(config)
	}
	if err != nil {
		return nil, err
	}
	return host, nil
}

type installPaths struct {
	instance                                             string
	root, installerState, runtimeState                   string
	worker, workerNext, workerRollback, workerPrevious   string
	journal, metadata                                    string
	hostdToken, hostdSocket, updaterSocket, updateState  string
	environmentCredentialDirectory                       string
	environmentCredential, environmentCredentialMetadata string
}
type installJournal struct {
	PreviousMetadata []byte               `json:"previous_metadata"`
	MetadataCaptured bool                 `json:"metadata_captured"`
	Source           installsource.Source `json:"source"`
	Schema           string               `json:"schema"`
	Stage            string               `json:"stage"`
	HadWorker        bool                 `json:"had_worker"`
	UpdatedAt        time.Time            `json:"updated_at"`
}

func newInstallJournal(paths installPaths, source installsource.Source) (installJournal, error) {
	hadWorker, probeErr := checkRegularFile(paths.worker)
	if probeErr != nil && !installNotExistOnly(probeErr) {
		return installJournal{}, probeErr
	}
	journal := installJournal{Source: source, Schema: journalSchemaV1, Stage: "prepared", HadWorker: hadWorker, UpdatedAt: time.Now().UTC(), MetadataCaptured: true}
	info, err := os.Lstat(paths.metadata)
	if installNotExistOnly(err) {
		return journal, nil
	}
	if err != nil {
		return installJournal{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || ownerUID(info) != 0 || info.Size() > 128<<10 {
		return installJournal{}, ErrInvalidRequest
	}
	journal.PreviousMetadata, err = readProtectedInstallState(paths.metadata, 128<<10)
	return journal, err
}

func platformPaths(uid int) installPaths {
	layout, err := service.UserLayout(runtime.GOOS, uid)
	if err != nil {
		panic(hostInstallFailure("unsupported Unix service layout", err))
	}
	root := layout.InstallRoot
	instance := "u" + strconv.Itoa(uid)
	installerState, runtimeState := filepath.Join("/var/lib/paperboat-installer/users", instance), filepath.Join("/var/lib/paperboat/users", instance)
	if runtime.GOOS == "darwin" {
		installerState = filepath.Join("/Library/Application Support/Paperboat/users", instance)
		runtimeState = installerState
	}
	p := installPaths{instance: instance, root: root, installerState: installerState, runtimeState: runtimeState}
	p.worker = layout.Binary
	// Release slots are kept in a dedicated root-owned directory so the
	// updater can atomically stage, promote, and roll back without touching the
	// command path or relying on a caller-controlled location.
	releases := layout.ReleasesRoot
	p.workerNext = layout.BinaryStaged
	p.workerRollback = layout.BinaryRollback
	p.workerPrevious = filepath.Join(releases, "pb.previous")
	p.journal = filepath.Join(installerState, "install-journal.json")
	p.metadata = filepath.Join(installerState, "install-metadata.json")
	p.hostdToken = filepath.Join(runtimeState, "hostd.token")
	p.hostdSocket = layout.HostdSocket
	p.updaterSocket = layout.UpdaterSocket
	p.updateState = layout.UpdateStateRoot
	if runtime.GOOS != "darwin" {
		p.environmentCredentialDirectory = filepath.Join(installerState, "environment")
		p.environmentCredential = filepath.Join(p.environmentCredentialDirectory, "host-key.cred")
		p.environmentCredentialMetadata = filepath.Join(p.environmentCredentialDirectory, "host-key.json")
	}
	return p
}

// LoadEnrolledOwner returns the committed, root-protected installation
// identity for the requested Unix account. The privileged updater uses this
// to launch owner-scoped readiness probes without trusting caller input.
func LoadEnrolledOwner(uid int) (Request, error) {
	if os.Geteuid() != 0 {
		return Request{}, ErrNotPrivileged
	}
	return loadInstallMetadata(platformPaths(uid).metadata, uid)
}

func ensureHostdToken(paths installPaths, request Request) error {
	// Hostd runs as the enrolled user, while the token directory is root-owned.
	// Keep the directory non-writable to that user but traversable so hostd can
	// read its 0600 token file.
	if err := secureRootDirectory(filepath.Dir(paths.hostdToken), 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(paths.hostdToken)
	if installNotExistOnly(err) {
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		if err := os.WriteFile(paths.hostdToken, token, 0o600); err != nil {
			return err
		}
	} else if err != nil {
		return hostInstallFailure("inspect existing runtime token", err)
	} else if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return ErrInvalidRequest
	}
	if err := os.Chown(paths.hostdToken, request.UID, request.GID); err != nil {
		return err
	}
	return os.Chmod(paths.hostdToken, 0o600)
}

func ensureSharedUserParents(paths installPaths) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	users := filepath.Dir(paths.runtimeState)
	root := filepath.Dir(users)
	for _, path := range []string{root, users} {
		if err := secureRootDirectory(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func ensureManagedDirectories(paths installPaths, request Request) error {
	// hostd runs as the enrolled account and must be able to create its
	// authenticated socket and fence state. The updater remains root-owned.
	for _, directory := range []string{filepath.Dir(paths.hostdSocket), filepath.Join(paths.runtimeState, "hostd")} {
		if err := secureManagedUserDirectory(directory, 0o700, request.UID, request.GID); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Dir(paths.updaterSocket), 0o755},
		{paths.updateState, 0o700},
		{filepath.Join(paths.root, "releases"), 0o755},
		{filepath.Dir(paths.worker), 0o755},
	} {
		if err := secureRootDirectory(item.path, item.mode); err != nil {
			return err
		}
	}
	return nil
}

func ensureEnvironmentHostCredential(ctx context.Context, paths installPaths, request Request) (bool, error) {
	if request.Platform != "linux" {
		return false, nil
	}
	if request.InstallationGeneration < 1 {
		return false, ErrInvalidRequest
	}
	return environmentkey.EnsureSystemdCredential(ctx, environmentkey.ProvisionConfig{
		CiphertextPath: paths.environmentCredential,
		MachineID:      request.UserMachineID, Generation: uint64(request.InstallationGeneration),
	})
}

// secureManagedUserDirectory accepts the two owners that can safely have
// created a runtime socket directory: root from a prior installation, or the
// enrolled account from a running hostd. It rejects every other owner and
// never follows symlinks before transferring ownership to the enrolled user.
func secureManagedUserDirectory(path string, mode os.FileMode, uid, gid int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if installNotExistOnly(err) {
		if err := os.MkdirAll(path, mode); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return ErrInvalidRequest
	}
	owner := ownerUID(info)
	if owner != 0 && owner != uid {
		return ErrInvalidRequest
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func writeInstallMetadata(path string, request Request) error {
	if err := secureRootDirectory(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: os.Geteuid(), OwnerGID: -1})
}

func loadInstallMetadata(path string, sudoUID int) (Request, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Request{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || ownerUID(info) != 0 || info.Size() < 1 || info.Size() > 128<<10 {
		return Request{}, ErrInvalidRequest
	}
	body, err := readProtectedInstallState(path, 128<<10)
	if err != nil {
		return Request{}, err
	}
	request, err := Decode(strings.NewReader(string(body)))
	if err != nil {
		return Request{}, hostInstallFailure("reading installation state failed", ErrInvalidRequest, err)
	}
	if request.Schema != SchemaV1 || request.Platform != runtime.GOOS || !validRunIdentity(request) || request.UID != sudoUID {
		return Request{}, ErrInvalidRequest
	}
	account, err := user.Lookup(request.User)
	if err != nil {
		return Request{}, hostInstallFailure("reading installation state failed", ErrInvalidRequest, err)
	}
	if account.Uid != strconv.Itoa(request.UID) || account.Gid != strconv.Itoa(request.GID) || account.HomeDir != request.Home {
		return Request{}, ErrInvalidRequest
	}
	group, err := user.LookupGroup(request.Group)
	if err != nil {
		return Request{}, hostInstallFailure("reading installation state failed", ErrInvalidRequest, err)
	}
	if group.Gid != strconv.Itoa(request.GID) {
		return Request{}, ErrInvalidRequest
	}
	if !request.EnrollmentPending && request.InstallationGeneration < 1 {
		return Request{}, ErrInvalidRequest
	}
	return request, nil
}

func stageBinary(source, destination string, identity installsource.Source) (resultErr error) {
	if err := identity.Verify(source); err != nil {
		return err
	}
	if err := secureRootDirectory(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("installation cleanup failed", resultErr, input.Close()) }()
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=streamed-binary-staging
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".binary-*")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer func() {
		resultErr = withCleanup("staged installation cleanup failed", resultErr, removeInstallFile(path))
	}()
	if err := temporary.Chmod(0o755); err != nil {
		return hostInstallFailure("staging installation failed", err, temporary.Close())
	}
	written, err := io.Copy(temporary, io.LimitReader(input, 256<<20+1))
	if err != nil || written < 1 || written > 256<<20 {
		return hostInstallFailure("staging installation failed", ErrInvalidRequest, err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return hostInstallFailure("staging installation failed", err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := identity.Verify(path); err != nil {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=verified-stage-publication
	if err := os.Rename(path, destination); err != nil {
		return err
	}
	return nil
}

func activateBinary(current, next, rollback string) error {
	if err := os.Remove(rollback); err != nil && !installNotExistOnly(err) {
		return err
	}
	if info, err := os.Lstat(current); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || ownerUID(info) != 0 {
			return ErrInvalidRequest
		}
		//paperboat:allow-source-policy atomic-replacement owner=host-install reason=current-binary-rollback-stage
		if err := os.Rename(current, rollback); err != nil {
			return err
		}
	} else if !installNotExistOnly(err) {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=verified-binary-activation
	return os.Rename(next, current)
}

func rollbackFiles(paths installPaths, journal installJournal) error {
	var result error
	for _, item := range []struct {
		current, rollback string
		had               bool
	}{{paths.worker, paths.workerRollback, journal.HadWorker}} {
		rollbackExists, probeErr := checkRegularFile(item.rollback)
		if probeErr != nil && !installNotExistOnly(probeErr) {
			result = hostInstallFailure("installation rollback inspection failed", result, probeErr)
			continue
		}
		if rollbackExists || !item.had {
			if err := os.Remove(item.current); err != nil && !installNotExistOnly(err) {
				result = errors.Join(result, err)
			}
		}
		if item.had && rollbackExists {
			//paperboat:allow-source-policy atomic-replacement owner=host-install reason=journaled-install-rollback
			if err := os.Rename(item.rollback, item.current); err != nil {
				result = errors.Join(result, err)
			}
		} else if !item.had {
			result = hostInstallFailure("installation rollback cleanup failed", result, removeInstallFile(item.rollback))
		}
	}
	result = hostInstallFailure("installation rollback cleanup failed", result, removeInstallFile(paths.workerNext))
	if result != nil {
		return result
	}
	if journal.MetadataCaptured {
		if len(journal.PreviousMetadata) > 0 {
			if err := atomicfile.Write(paths.metadata, journal.PreviousMetadata, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: -1}); err != nil {
				return err
			}
		} else if err := os.Remove(paths.metadata); err != nil && !installNotExistOnly(err) {
			return err
		}
	}
	if err := updated.RollbackUnixNativeInstall(paths.updateState); err != nil {
		return err
	}
	return removeJournal(paths.journal)
}

func recoverInterrupted(paths installPaths) (bool, error) {
	journal, err := loadJournal(paths.journal)
	if installNotExistOnly(err) {
		return false, updated.CommitUnixNativeInstall(paths.updateState)
	}
	if err != nil {
		return false, err
	}
	return true, rollbackFiles(paths, journal)
}

func writeJournal(path string, journal installJournal) error {
	if err := secureRootDirectory(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: os.Geteuid(), OwnerGID: -1})
}

func loadJournal(path string) (installJournal, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return installJournal{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 256<<10 || ownerUID(info) != 0 {
		return installJournal{}, ErrInvalidRequest
	}
	body, err := readProtectedInstallState(path, 256<<10)
	if err != nil {
		return installJournal{}, err
	}
	var journal installJournal
	if err := decodeInstallJSON(strings.NewReader(string(body)), &journal, 256<<10); err != nil {
		return installJournal{}, err
	}
	if journal.Schema != journalSchemaV1 || !slices.Contains([]string{"prepared", "binaries_activated", "services_started"}, journal.Stage) {
		return installJournal{}, ErrInvalidRequest
	}
	return journal, nil
}

func replacePrevious(rollback, previous string) error {
	if _, err := os.Lstat(rollback); installNotExistOnly(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.Remove(previous); err != nil && !installNotExistOnly(err) {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=verified-previous-retention
	return os.Link(rollback, previous)
}
func removeJournal(path string) (resultErr error) {
	err := os.Remove(path)
	if installNotExistOnly(err) {
		return nil
	}
	if err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("installation cleanup failed", resultErr, directory.Close()) }()
	return directory.Sync()
}
func removeInstalledFiles(ctx context.Context, paths installPaths) error {
	keep, _ := ctx.Value(resetCLIContextKey{}).(string)
	var result error
	carrierSocket := ""
	if paths.hostdSocket != "" {
		carrierSocket = paths.hostdSocket + ".carrier"
	}
	for _, path := range []string{
		paths.worker, paths.workerNext, paths.workerRollback, paths.workerPrevious,
		paths.metadata,
		paths.hostdToken, paths.hostdSocket, carrierSocket, paths.updaterSocket,
		filepath.Join(paths.runtimeState, "hostd", "fence.json"),
		filepath.Join(paths.runtimeState, "hostd"),
		paths.environmentCredential, paths.environmentCredentialMetadata,
		filepath.Join(paths.runtimeState, "power-baseline.json"),
		filepath.Join(paths.runtimeState, "availability-policy.json"),
		filepath.Join(paths.runtimeState, "update-current.json"),
		filepath.Join(paths.runtimeState, "update-journal.json"),
		filepath.Join(paths.runtimeState, "update-rollbacks.json"),
		filepath.Join(paths.runtimeState, "privileged-updates", "update-current.json"),
		filepath.Join(paths.runtimeState, "privileged-updates", "update-journal.json"),
		filepath.Join(paths.runtimeState, "privileged-updates", "update-rollbacks.json"),
		filepath.Join(paths.runtimeState, "privileged-updates"),
	} {
		if path == "" || path == keep {
			continue
		}
		if err := os.Remove(path); err != nil && !installNotExistOnly(err) {
			result = errors.Join(result, err)
		}
	}
	for _, directory := range []string{paths.environmentCredentialDirectory, paths.updateState, filepath.Dir(paths.updaterSocket), filepath.Join(paths.root, "releases")} {
		if directory == "" || directory == "." || directory == string(filepath.Separator) || !filepath.IsAbs(directory) {
			continue
		}
		if err := RemovePathPreservingCLI(ctx, directory, keep); err != nil {
			result = errors.Join(result, err)
		}
	}
	// Preserved account-private control directories are removed only after
	// every owning native role is uninstalled. Never recursively delete an
	// unexpected entry in this shared lifetime namespace.
	if runtime.GOOS == "darwin" && paths.hostdSocket != "" {
		if err := os.RemoveAll(filepath.Dir(paths.hostdSocket)); err != nil {
			result = errors.Join(result, err)
		}
	}
	if runtime.GOOS == "linux" && paths.hostdSocket != "" {
		uid, err := carrierUID(paths.instance)
		if err != nil {
			return errors.Join(result, err)
		}
		if paths.hostdSocket != platformPaths(uid).hostdSocket {
			return errors.Join(result, ErrInvalidRequest)
		}
		for _, directory := range []string{filepath.Dir(paths.hostdSocket), filepath.Join("/run", "paperboat-carrier-"+paths.instance)} {
			info, err := os.Lstat(directory)
			if installNotExistOnly(err) {
				continue
			}
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || ownerUID(info) != uid {
				result = errors.Join(result, ErrInvalidRequest)
				continue
			}
			if err := os.Remove(directory); err != nil && !installNotExistOnly(err) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}
func regularFile(path string) bool {
	regular, err := checkRegularFile(path)
	return err == nil && regular
}
func checkRegularFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, ErrInvalidRequest
	}
	return true, nil
}

// lockUnixInstallState owns the directory required before activation locking.
func lockUnixInstallState(ctx context.Context, root string) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInvalidRequest
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		// Validate custody before creating only the lock infrastructure. A shared
		// sticky system temp ancestor is safe when its descendant is root-owned.
		for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
			info, err := os.Lstat(parent)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || ownerUID(info) != 0 || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
				return nil, ErrInvalidRequest
			}
			if parent == string(filepath.Separator) {
				break
			}
		}
		if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	// Existing entries remain subject to the activation owner's strict checks;
	// never chmod or repair unexpected state to obtain admission.
	return updated.LockUnixNativeInstall(ctx, root)
}

func secureRootDirectory(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || ownerUID(info) != 0 {
		return ErrInvalidRequest
	}
	return os.Chmod(path, mode)
}

func Validate(request Request, sudoUID int) error {
	if request.Schema != SchemaV1 || request.Platform != runtime.GOOS || !validRunIdentity(request) || sudoUID != request.UID ||
		request.EnrollmentPending || request.UserMachineID == "" || strings.ContainsAny(request.UserMachineID, "\x00\r\n") ||
		request.InstallationGeneration < 1 {
		return hostInstallFailure("identity contract", ErrInvalidRequest)
	}
	account, err := user.Lookup(request.User)
	if err != nil {
		return hostInstallFailure("user account", ErrInvalidRequest, err)
	}
	if account.Uid != strconv.Itoa(request.UID) || account.Gid != strconv.Itoa(request.GID) || account.HomeDir != request.Home {
		return hostInstallFailure("user account", ErrInvalidRequest)
	}
	group, err := user.LookupGroup(request.Group)
	if err != nil {
		return hostInstallFailure("user group", ErrInvalidRequest, err)
	}
	if group.Gid != strconv.Itoa(request.GID) {
		return hostInstallFailure("user group", ErrInvalidRequest)
	}
	if err := bootstrap.VerifyArtifactTarget(request.Artifact); request.Source.Distribution == installsource.Official && (err != nil || request.Artifact.Platform != request.Platform) {
		return hostInstallFailure("TUF target descriptor", ErrInvalidRequest, err)
	}
	if err := verifyArtifact(request.Executable, request.UID, request.Platform); err != nil {
		return hostInstallFailure("supplied executable file", ErrInvalidRequest, err)
	}
	if err := request.Source.Verify(request.Executable); err != nil || request.Source.Platform != request.Platform || request.Source.Architecture != runtime.GOARCH {
		return hostInstallFailure("target architecture", ErrInvalidRequest, err)
	}
	for _, path := range []string{request.Home, request.StateRoot, request.WorkspaceRoot} {
		if err := checkCanonicalOwnedDirectory(path, request.UID); err != nil {
			return hostInstallFailure("invalid installation directory", ErrInvalidRequest, err)
		}
	}
	if err := checkCanonicalExecutable(request.Shell); err != nil {
		return hostInstallFailure("invalid login shell", ErrInvalidRequest, err)
	}
	if !pathListValid(request.Path) {
		return hostInstallFailure("service PATH", ErrInvalidRequest)
	}
	parsed, err := url.Parse(request.ControlURL)
	if err != nil {
		return hostInstallFailure("control URL", ErrInvalidRequest, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return hostInstallFailure("control URL", ErrInvalidRequest)
	}
	host, port, err := net.SplitHostPort(request.HelperListenAddress)
	if err != nil {
		return hostInstallFailure("helper listen address", ErrInvalidRequest, err)
	}
	if port == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return hostInstallFailure("helper listen address", ErrInvalidRequest)
	}
	return nil
}

func validRunIdentity(request Request) bool {
	return request.UID > 0 && request.GID > 0 || request.UID == 0 && request.GID == 0 && request.User == "root"
}

func workerEnvironment(request Request) map[string]string {
	controlHost := ""
	if parsed, err := url.Parse(request.ControlURL); err == nil {
		controlHost = parsed.Hostname()
	}
	releaseHost := ""
	if parsed, err := url.Parse(request.Artifact.RepositoryURL); err == nil {
		releaseHost = parsed.Hostname()
	}
	directHosts := controlHost
	if releaseHost != "" && releaseHost != controlHost {
		directHosts += "," + releaseHost
	}
	return map[string]string{
		"HOME": request.Home, "PATH": request.Path,
		"PAPERBOAT_RUNTIME_STATE_ROOT": request.StateRoot, "PAPERBOAT_WORKSPACE_ROOT": request.WorkspaceRoot,
		"PAPERBOAT_SESSION_OWNER_CATALOG": filepath.Join(platformPaths(request.UID).root, "session-owners"),
		"PAPERBOAT_CONTROL_URL":           request.ControlURL, "PAPERBOAT_MACHINE_ID": request.UserMachineID,
		"PAPERBOAT_SHELL": request.Shell, "PAPERBOAT_RUNTIME_LISTEN_ADDRESS": request.HelperListenAddress,
		"PAPERBOAT_RUNTIME_SERVICE_SCOPE": "system",
		// Paperboat's own narrow private-preview PAC applies to browsers. The
		// managed control plane must remain direct so enabling that PAC cannot
		// prevent hostd/updater from starting.
		"PAPERBOAT_NO_PROXY": directHosts,
	}
}

func invokingUID() int {
	value := os.Getenv("SUDO_UID")
	if value == "" {
		value = os.Getenv("PAPERBOAT_INVOKING_UID")
	}
	uid, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return uid
}

func verifyArtifact(path string, uid int, platform string) (resultErr error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	lstat, err := os.Lstat(path)
	rootOwnedSource := lstat != nil && ownerUID(lstat) == 0
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if !lstat.Mode().IsRegular() || lstat.Mode()&os.ModeSymlink != 0 || lstat.Mode().Perm()&0o022 != 0 || (ownerUID(lstat) != uid && !rootOwnedSource) {
		return ErrInvalidRequest
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("installation cleanup failed", resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(lstat, info) || info.Mode().Perm()&0o022 != 0 || info.Size() < 1 || info.Size() > 256<<20 || (ownerUID(info) != uid && ownerUID(info) != 0) {
		return ErrInvalidRequest
	}
	return nil
}

func canonicalOwnedDirectory(path string, uid int) bool {
	return checkCanonicalOwnedDirectory(path, uid) == nil
}
func checkCanonicalOwnedDirectory(path string, uid int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if resolved != path {
		return ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || ownerUID(info) != uid {
		return ErrInvalidRequest
	}
	return nil
}
func canonicalExecutable(path string) bool { return checkCanonicalExecutable(path) == nil }
func checkCanonicalExecutable(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return ErrInvalidRequest
	}
	return nil
}

func pathListValid(value string) bool {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, path := range filepath.SplitList(value) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return false
		}
	}
	return true
}

func ownerUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func (r Request) String() string {
	return fmt.Sprintf("%s for uid %d", r.Schema, r.UID)
}

func uninstallWithActivation(ctx context.Context, retire, uninstall func(context.Context) error) error {
	if err := retire(ctx); err != nil {
		return withUninstallStage("uninstall_activator", err)
	}
	return withUninstallStage("uninstall_services", uninstall(ctx))
}
