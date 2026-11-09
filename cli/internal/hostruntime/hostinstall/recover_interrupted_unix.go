//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
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
// component (the activation helper), which restores them all.
//
// The updater service and the activation helper may both start a recovery: the one that finds the other already
// recovering gets ErrRecoveryBusy and must not read the installation, which is half restored until the other is done.
func RecoverInterrupted(ctx context.Context, uid int, self string) (Recovery, error) {
	if os.Geteuid() != 0 {
		return Recovery{}, ErrNotPrivileged
	}
	if uid < 0 {
		return Recovery{}, ErrInvalidRequest
	}
	paths := platformPaths(uid)
	return recoverInterruptedInstall(ctx, paths, uid, service.ExecRunner{}, func(ctx context.Context, request Request) (func(context.Context) error, error) {
		if request.Schema == "" {
			return nil, nil // nothing records which services to put back
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

// ErrRecoveryBusy is another process undoing the same interrupted installation right now.
var ErrRecoveryBusy = errors.New("another Paperboat process is undoing the interrupted installation; retry shortly")

// Recovery is what RecoverInterrupted did.
type Recovery struct {
	// FinishSelf, when set, is left for the caller's own component once everything else is undone
	// (service.LifecycleManager.RecoverWithin): the caller runs it as its very last step, then exits.
	FinishSelf func(context.Context) error
	// Undone: an interrupted installation was found and its files and services restored.
	Undone bool
	// InstallRunning: the journal belongs to an installer that is still running; nothing was recovered.
	InstallRunning bool
	// Cleanup is what could not be withdrawn of the killed attempt's candidate (withdrawInterruptedCandidate). The
	// recovery itself completed: a leftover is reported, never a reason to leave the services unrestored.
	Cleanup error
}

func recoverInterruptedInstall(ctx context.Context, paths installPaths, uid int, runner service.Runner, restoreServices func(context.Context, Request) (func(context.Context) error, error)) (result Recovery, resultErr error) {
	if _, err := os.Lstat(paths.journal); installNotExistOnly(err) {
		return Recovery{}, nil
	} else if err != nil {
		return Recovery{}, err
	}
	// Taken before the install lock and only by recoverers, so a held install lock without it is a live installer.
	recovering, err := lockInterruptedRecovery(paths.updateState)
	if err != nil {
		return Recovery{}, err
	}
	defer recovering.Close()
	waitCtx, cancel := context.WithTimeout(ctx, interruptedInstallWriterWait)
	defer cancel()
	lock, err := updated.LockUnixNativeInstall(waitCtx, paths.updateState)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, updated.ErrActivationPending) {
			return Recovery{InstallRunning: true}, nil // the installer is still running and owns its journal
		}
		return Recovery{}, err
	}
	defer func() {
		if resultErr = withCleanup("interrupted installation recovery cleanup failed", resultErr, lock.Close()); resultErr != nil {
			result = Recovery{} // something is left to retry: the updater stays up for its next start
		}
	}()
	journal, err := loadJournal(paths.journal)
	if installNotExistOnly(err) {
		return Recovery{}, nil // committed while this waited for the lock
	}
	if err != nil {
		return Recovery{}, err
	}
	// The lifecycle recovery of a replace restores the executable from its verified pin and the metadata BEFORE it
	// starts any previous service (service.LifecycleManager.rollback calls nativeInstallation.Restore first), and ends
	// by removing this journal. Rolling the files back before it would remove the running executable's path for
	// nothing; after it, the journal is gone and only the leftover rollback copy of the worker remains. Every step is
	// repeatable: until the journal is removed, a run that was interrupted anywhere starts over from the top.
	// First: nothing the killed attempt staged for its version stays. It is cleanup: what cannot be proven to be the
	// candidate's (an unreadable descriptor) stays and is reported, and the services are restored regardless.
	request, active, err := recoveryRequest(paths, uid, journal)
	var cleanup error
	if errors.Is(err, errPendingUnreadable) {
		cleanup, err = err, nil // the files are still undone; the unprovable services and candidate stay, reported
	}
	if err != nil {
		return Recovery{}, err
	}
	cleanup = errors.Join(cleanup, withdrawInterruptedCandidate(ctx, paths, request, active, runner, journal.Source))
	finishSelf, err := restoreServices(ctx, request)
	if err != nil {
		return Recovery{}, err
	}
	if _, err = os.Lstat(paths.journal); installNotExistOnly(err) {
		return Recovery{FinishSelf: finishSelf, Cleanup: cleanup, Undone: true}, errors.Join(removeInstallFile(paths.workerRollback), removeInstallFile(paths.workerNext))
	} else if err != nil {
		return Recovery{}, err
	}
	// No lifecycle journal was left (or no metadata to rebuild services from): only the files need undoing.
	return Recovery{FinishSelf: finishSelf, Cleanup: cleanup, Undone: true}, rollbackFiles(paths, journal)
}

// lockInterruptedRecovery is held by whichever of the updater service and the activation helper recovers;
// ErrRecoveryBusy when the other one does. flock is released when the holder exits, however it exits.
func lockInterruptedRecovery(root string) (io.Closer, error) {
	file, err := os.OpenFile(filepath.Join(root, "interrupted-recovery.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRecoveryBusy
		}
		return nil, errors.Join(err, closeErr)
	}
	return file, nil
}

// recoveryRequest is the installation a recovery returns to and its active source: the installed record, or for a
// killed first enrollment the request it was installing, with no active source (nothing of it stays). The zero
// Request when neither exists (a journal from before Pending was recorded).
func recoveryRequest(paths installPaths, uid int, journal installJournal) (Request, string, error) {
	installed, err := loadInstallMetadata(paths.metadata, uid)
	if err == nil {
		return installed, installed.Source.SHA256, nil
	}
	if !installNotExistOnly(err) {
		return Request{}, "", err
	}
	if len(journal.Pending) == 0 {
		return Request{}, "", nil
	}
	pending, err := decodeInstallRequest(journal.Pending, uid)
	if err != nil {
		return Request{}, "", errors.Join(errPendingUnreadable, err)
	}
	return pending, "", nil
}

var errPendingUnreadable = errors.New("the interrupted first enrollment's request could not be read; its services and session owner stay")

// withdrawInterruptedCandidate undoes the session owner a killed install started for its candidate source and the
// candidate's pin, as the install's own failure path does (candidateSessionOwner.withdraw, then the inventory): the
// inventory cannot tell that descriptor from history once recovery reactivates the previous worker (pbvm1 F14-03:
// both stayed through a later good update). request and active are recoveryRequest's. It runs under the install
// lock, so no install is staging a pin. A descriptor of that generation on other code is history republished on the
// active code, not the candidate.
func withdrawInterruptedCandidate(ctx context.Context, paths installPaths, request Request, active string, runner service.Runner, candidate installsource.Source) error {
	if request.Schema == "" || active == candidate.SHA256 {
		return nil
	}
	catalog := sessionOwnerCatalogAt(paths, request)
	catalog.Runner = runner
	if _, err := os.Lstat(catalog.Root); installNotExistOnly(err) {
		return nil // no catalogue: no session owner was ever staged
	}
	return errors.Join(withdrawCandidateOwner(ctx, paths.metadata, request.UID, catalog, candidate),
		pruneSessionOwnerPinsAt(ctx, catalog, paths, request.UID, request.StateRoot, active))
}

// withdrawCandidateOwner withdraws the session owner of candidate unless its descriptor is history republished on
// other code.
func withdrawCandidateOwner(ctx context.Context, metadata string, uid int, catalog sessionowner.Catalog, candidate installsource.Source) error {
	generation, err := sourceGeneration(candidate)
	if err != nil {
		return err
	}
	path, err := sessionowner.Find(catalog.Root, generation)
	if err != nil {
		return err
	}
	d, err := sessionowner.Read(path)
	owner := candidateSessionOwner{catalog: catalog, generation: generation, existed: err == nil && d.Source.SHA256 != candidate.SHA256}
	return owner.withdraw(ctx, metadata, uid)
}
