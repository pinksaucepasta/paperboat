//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

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
	var notServing *carrierNotServingError
	replaceCarrier := errors.As(err, &notServing)
	if replaceCarrier {
		err = nil
	}
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
			if err := ensureUserLinger(ctx, request, paths); err != nil {
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
			request, err = activatePreparedCandidate(request, paths)
			if err != nil {
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
		} else if replaceCarrier {
			if err := replaceUnservingUnixCarrier(ctx, request, paths); err != nil {
				return hostInstallFailure("the installed carrier was not serving and its replacement did not become ready; restoring the previous version", err)
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
		if intent == unixSuppliedInstall && hooks.Validate != nil {
			// Nothing is committed yet, so failing here restores the previous installation.
			if err := hooks.Validate(ctx); err != nil {
				return hostInstallFailure("the new version did not become ready; restoring the previous version", ErrUpdateNotReady, err)
			}
		}
		return commitPreparedInstallationLocked(ctx, request, paths, true)
	})
	if resultErr == nil && intent == unixSuppliedInstall {
		return finalizePreparedUpdateLocked(ctx, request.UID)
	}
	return resultErr
}
