//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"runtime"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

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
