//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

// interruptedInstallWriterWait bounds how long recovery waits for a writer that may still be running.
var interruptedInstallWriterWait = 3 * time.Second

// RecoverInterrupted undoes an install whose process died (a killed client, a lost session, a power cut). Such an
// install leaves its journal at an early stage; every reader of that journal (the updater's readiness probe) rejects
// it, so the updater crash-looped until somebody ran another install. The installer holds the native install lock for
// its whole run, so a journal found while the lock is free has no writer: the interrupted services and files are
// restored and the journal removed, once. A live writer keeps its journal and is left alone.
//
// self is the lifecycle component the caller runs as (the updater service), or "" for a caller outside every
// component (the activation helper), which restores them all. The caller's own service must not be stopped by the
// recovery: a non-nil finishSelf (service.LifecycleManager.RecoverWithin) is returned only once everything else is
// undone, and the caller runs it as its very last step, then exits.
func RecoverInterrupted(ctx context.Context, uid int, self string) (finishSelf func(context.Context) error, err error) {
	if os.Geteuid() != 0 {
		return nil, ErrNotPrivileged
	}
	if uid < 0 {
		return nil, ErrInvalidRequest
	}
	paths := platformPaths(uid)
	return recoverInterruptedInstall(ctx, paths, func(ctx context.Context) (func(context.Context) error, error) {
		request, err := loadInstallMetadata(paths.metadata, uid)
		if installNotExistOnly(err) {
			return nil, nil // a first enrollment has no earlier services to put back
		}
		if err != nil {
			return nil, err
		}
		hostd, updater, err := pendingInstallers(request, paths)
		if err != nil {
			return nil, errors.Join(ErrInvalidRequest, err)
		}
		host, err := hostInstallerWithMissing(request, paths, true)
		if err != nil {
			return nil, errors.Join(ErrInvalidRequest, err)
		}
		lifecycle, err := newLifecycleManager(request, paths, host, hostd, updater)
		if err != nil {
			return nil, errors.Join(ErrInvalidRequest, err)
		}
		return lifecycle.RecoverWithin(ctx, self)
	})
}

func recoverInterruptedInstall(ctx context.Context, paths installPaths, restoreServices func(context.Context) (func(context.Context) error, error)) (finishSelf func(context.Context) error, resultErr error) {
	if _, err := os.Lstat(paths.journal); installNotExistOnly(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, interruptedInstallWriterWait)
	defer cancel()
	lock, err := updated.LockUnixNativeInstall(waitCtx, paths.updateState)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, updated.ErrActivationPending) {
			return nil, nil // the installer is still running and owns its journal
		}
		return nil, err
	}
	defer func() {
		if resultErr = withCleanup("interrupted installation recovery cleanup failed", resultErr, lock.Close()); resultErr != nil {
			finishSelf = nil // something is left to retry: the updater stays up for its next start
		}
	}()
	journal, err := loadJournal(paths.journal)
	if installNotExistOnly(err) {
		return nil, nil // committed while this waited for the lock
	}
	if err != nil {
		return nil, err
	}
	// The lifecycle recovery of a replace restores the executable from its verified pin and the metadata BEFORE it
	// starts any previous service (service.LifecycleManager.rollback calls nativeInstallation.Restore first), and ends
	// by removing this journal. Rolling the files back before it would remove the running executable's path for
	// nothing; after it, the journal is gone and only the leftover rollback copy of the worker remains. Every step is
	// repeatable: until the journal is removed, a run that was interrupted anywhere starts over from the top.
	finishSelf, err = restoreServices(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = os.Lstat(paths.journal); installNotExistOnly(err) {
		return finishSelf, errors.Join(removeInstallFile(paths.workerRollback), removeInstallFile(paths.workerNext))
	} else if err != nil {
		return nil, err
	}
	// No lifecycle journal was left (or no metadata to rebuild services from): only the files need undoing.
	return finishSelf, rollbackFiles(paths, journal)
}
