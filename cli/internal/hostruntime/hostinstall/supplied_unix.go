//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

// SuppliedBinary uses the same fixed paths, binary journal and enrolled service
// transaction as enrollment. An unbound installation starts only the user daemon;
// account-dependent roles are installed when enrollment supplies their identity.
func SuppliedBinary(ctx context.Context, input Request, operation string) (resultErr error) {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if input.Schema != SchemaV1 || input.Platform != runtime.GOOS || input.UID != invokingUID() || !validRunIdentity(input) {
		return ErrInvalidRequest
	}
	account, err := user.LookupId(strconv.Itoa(input.UID))
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if account.Username != input.User || account.HomeDir != input.Home || account.Gid != strconv.Itoa(input.GID) {
		return ErrInvalidRequest
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if group.Name != input.Group {
		return ErrInvalidRequest
	}
	if input.UpdateBundle == "" {
		if input.SuppliedUpdate {
			input.Source.Distribution = installsource.Custom
			input.Source.AutomaticUpdates = false
		}
		if input.Source.Platform != runtime.GOOS || input.Source.Architecture != runtime.GOARCH {
			return ErrInvalidRequest
		}
		if err = verifyArtifact(input.Executable, input.UID, input.Platform); err != nil {
			return err
		}
		if err = input.Source.Verify(input.Executable); err != nil {
			return err
		}
	}
	paths := platformPaths(input.UID)
	if err := ensureSharedUserParents(paths); err != nil {
		return err
	}
	if err := secureRootDirectory(filepath.Dir(paths.metadata), 0755); err != nil {
		return err
	}
	if err = secureRootDirectory(paths.updateState, 0700); err != nil {
		return err
	}
	nativeLock, err := updated.LockUnixNativeInstall(ctx, paths.updateState)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = withCleanup("native installation lock cleanup failed", resultErr, nativeLock.Close())
	}()
	// Signed activation already holds this lock before taking metadata custody.
	lock, err := LockSuppliedOperation(paths.metadata+".lock", 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("installation lock cleanup failed", resultErr, lock.Close()) }()
	if operation == "install-supplied" && input.UpdateBundle != "" {
		resolved, cleanup, resolveErr := ResolveUpdateBundle(ctx, input, paths.updateState)
		if resolveErr != nil {
			return resolveErr
		}
		defer cleanup()
		input = resolved
	}
	// Recover the protected native owner before using an installation record
	// to bind the candidate. Recovery can restore a different prior source or
	// restart legacy roles; the later cold preflight must inspect that result.
	previous, err := loadInstallMetadata(paths.metadata, input.UID)
	if err != nil && !installNotExistOnly(err) {
		return err
	}
	if err == nil && !previous.EnrollmentPending {
		if e := prepareUnixNativeRecovery(ctx, paths, input.UID, operation == "install-supplied"); e != nil {
			return e
		}
	}
	if err == nil && !previous.EnrollmentPending {
		hostd, updater, e := pendingInstallers(previous, paths)
		if e != nil {
			return e
		}
		var host *service.Installer
		if runtime.GOOS != "darwin" {
			host, e = hostInstallerWithMissing(previous, paths, true)
			if e != nil {
				return e
			}
		}
		lifecycle, e := newLifecycleManager(previous, paths, host, hostd, updater)
		if e != nil {
			return e
		}
		if e = lifecycle.Recover(ctx); e != nil {
			return e
		}
		previous, err = loadInstallMetadata(paths.metadata, input.UID)
		if err != nil && !installNotExistOnly(err) {
			return err
		}
	}
	journal, journalErr := loadJournal(paths.journal)
	if journalErr != nil && !installNotExistOnly(journalErr) {
		return journalErr
	}
	if journalErr == nil && input.UpdateBundle != "" && operation != "install-supplied" {
		input.Source = journal.Source
		input.Executable = paths.worker
	}
	if journalErr == nil && journal.Source != input.Source {
		return errors.New("another Paperboat installation is pending; finish or recover that installation before replacing its source")
	}
	bound := err == nil && !previous.EnrollmentPending
	request := unboundSuppliedRequest(input)
	if bound {
		request = previous
		request.Executable, request.Source = input.Executable, input.Source
		if input.SuppliedUpdate && input.Source.Distribution == installsource.Official {
			if input.Artifact.Schema != "" {
				request.Artifact = input.Artifact
			} else {
				request.Artifact.Version = input.Source.Version
			}
		}
	} else {
		request.EnrollmentPending = true
	}
	switch operation {
	case "install-supplied":
		if bound {
			return installWithMaintenance(ctx, request, true, &input.Force)
		}

		if err = installMachineGuard(ctx, request); err != nil {
			return err
		}
		if err = ensureSharedUserParents(paths); err != nil {
			return err
		}
		if _, err = recoverInterrupted(paths); err != nil {
			return err
		}
		if err = secureRootDirectory(filepath.Dir(paths.worker), 0o755); err != nil {
			return err
		}
		if err = stageBinary(request.Executable, paths.workerNext, request.Source); err != nil {
			return err
		}
		journal, err := newInstallJournal(paths, request.Source)
		if err != nil {
			return err
		}
		if err = writeJournal(paths.journal, journal); err != nil {
			return err
		}
		if err = activateBinary(paths.worker, paths.workerNext, paths.workerRollback); err != nil {
			return errors.Join(err, rollbackFiles(paths, journal))
		}
		journal.Stage = "services_started"
		return writeJournal(paths.journal, journal)
	case "commit-supplied":
		request.Executable = paths.worker
		if err = request.Source.Verify(paths.worker); err != nil {
			return err
		}
		if bound {
			if err = Validate(request, invokingUID()); err != nil {
				return err
			}
			return commitPreparedInstallationLocked(ctx, request, paths, true)
		}
		return commitPreparedInstallationLocked(ctx, request, paths, true)
	case "rollback-supplied":
		journal, err := loadJournal(paths.journal)
		if err != nil {
			return err
		}
		if bound {
			// The current image may already be serving. Restoring its pathname
			// alone does not replace the running supervisor or its worker.
			if err = Stop(ctx, request); err != nil {
				return err
			}
		}
		if err = rollbackFiles(paths, journal); err != nil {
			return hostInstallFailure("Paperboat installation rollback is incomplete and runtime services may be stopped; retry pb install using the same installer executable to recover the retained installation journal", err)
		}
		if bound {
			restored, err := loadInstallMetadata(paths.metadata, input.UID)
			if err != nil {
				return err
			}
			restored.Executable = paths.worker
			return RestorePriorNativeRuntime(ctx, restored)
		}
		return nil
	default:
		return ErrInvalidRequest
	}
}

// LockSuppliedOperation serializes a caller's complete transaction, or a root
// helper's mutation, without leaving a stale lock after the process exits.
func LockSuppliedOperation(path string, uid int) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || ownerUID(info) != uid || info.Mode().Perm() != 0600 {
		return nil, hostInstallFailure("invalid installation lock", ErrInvalidRequest, err, f.Close())
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, hostInstallFailure("Paperboat installation lock could not be acquired; retry when the current installation finishes", err, f.Close())
	}
	return f, nil
}

// suppliedActiveRequest binds lifecycle admission to the current fixed executable slot;
// persisted enrollment metadata can still describe the pre-transaction image.
func suppliedActiveRequest(previous Request, paths installPaths) (Request, error) {
	previous.Executable = paths.worker
	journal, journalErr := loadJournal(paths.journal)
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return Request{}, journalErr
	}
	if err := previous.Source.Verify(paths.worker); err == nil {
		return previous, nil
	}
	// Even a prepared journal may already own the fixed image: activation's
	// rename precedes persistence of the binaries_activated stage. Only either
	// protected transaction identity may authorize stopping the current slot.
	if journalErr == nil {
		if err := journal.Source.Verify(paths.worker); err == nil {
			previous.Source = journal.Source
			return previous, nil
		}
	}
	return Request{}, ErrInvalidRequest
}

// unboundSuppliedRequest is the installation a supplied binary starts when this
// machine is not enrolled yet. It keeps the control URL: a fresh machine guard
// reads the deployment's local access domain from that server.
func unboundSuppliedRequest(input Request) Request {
	return Request{Schema: input.Schema, Platform: input.Platform, User: input.User, UID: input.UID, GID: input.GID, Group: input.Group, Home: input.Home, Executable: input.Executable, Source: input.Source, ControlURL: input.ControlURL, EnrollmentPending: true}
}
