//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseeligibility"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// healMachineGuard replaces a guard left on another version by an older updater (it installs the guard
// from this installation under the all-or-nothing undo), so no user has to run pb install after an
// update. It runs only while it holds the installation lock: an install or update in progress owns the
// guard and its undo, and a heal racing that undo left ubuntu24 without a running guard (2026-10-26).
// The lock is only tried, never waited on, since an install may be restarting this service while it
// holds it; a busy lock is tried again later. Failure leaves the guard as it was and is logged.
var (
	lockInstallation = updated.LockUnixNativeInstall
	healGuard        = hostinstall.HealMachineGuard
	healRetryWait    = 30 * time.Second
)

func healMachineGuard(ctx context.Context, stateRoot string, source installsource.Source, binary string, stderr io.Writer) {
	for attempt := 0; attempt < 20; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(healRetryWait):
			}
		}
		lockCtx, cancelLock := context.WithTimeout(ctx, time.Second)
		lock, err := lockInstallation(lockCtx, stateRoot)
		cancelLock()
		if err != nil {
			continue
		}
		healCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		healed, err := healGuard(healCtx, source, binary)
		cancel()
		_ = lock.Close()
		if err != nil {
			fmt.Fprintf(stderr, "paperboat-updated: machine guard runs another version and replacing it failed: %v\n", err)
		} else if healed {
			fmt.Fprintf(stderr, "paperboat-updated: machine guard ran another version; replaced it with %s\n", source.Version)
		}
		return
	}
	fmt.Fprintln(stderr, "paperboat-updated: machine guard version not checked; an installation held the lock for 10 minutes")
}

func runUpdated(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("updated", flag.ContinueOnError)
	flags.SetOutput(stderr)
	helper := flags.Bool("activation-helper", false, "run the protected native activation job")
	if flags.Parse(args) != nil || flags.NArg() != 0 || os.Geteuid() != 0 {
		return errors.New("invalid paperboat-updated invocation")
	}
	notifier, err := service.NewProcessNotifier()
	if err != nil {
		return err
	}
	if err := notifier.Starting(); err != nil {
		return err
	}
	failInitialization := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.Join(err, notifier.Degraded("updater initialization failed"))
	}
	stateRoot := os.Getenv("PAPERBOAT_UPDATE_STATE_ROOT")
	binary, binaryRollback, binaryStaged := os.Getenv("PAPERBOAT_BINARY"), os.Getenv("PAPERBOAT_BINARY_ROLLBACK"), os.Getenv("PAPERBOAT_BINARY_STAGED")
	socket, tokenPath, repository, machineID := os.Getenv("PAPERBOAT_HOSTD_SOCKET"), os.Getenv("PAPERBOAT_HOSTD_TOKEN_FILE"), os.Getenv("PAPERBOAT_RELEASE_REPOSITORY"), os.Getenv("PAPERBOAT_MACHINE_ID")
	controlSocket := os.Getenv("PAPERBOAT_UPDATED_SOCKET")
	healthURL := os.Getenv("PAPERBOAT_UPDATE_HEALTH_URL")
	uid, uidErr := strconv.Atoi(os.Getenv("PAPERBOAT_ENROLLED_UID"))
	gid, gidErr := strconv.Atoi(os.Getenv("PAPERBOAT_ENROLLED_GID"))
	paths := []string{stateRoot, binary, binaryRollback, binaryStaged, socket, tokenPath, controlSocket}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return failInitialization(errors.New("invalid paperboat-updated environment"))
		}
	}
	if machineID == "" || healthURL == "" || uidErr != nil || gidErr != nil || !validRuntimeIdentity(uid, gid) {
		return failInitialization(errors.New("invalid paperboat-updated environment"))
	}
	token, err := readWorkerToken(tokenPath)
	if err != nil {
		return failInitialization(err)
	}
	participants, err := newUnixUpdateParticipants(binary, uid, gid)
	if err != nil {
		return failInitialization(err)
	}
	hostdClient, err := hostdproto.NewClient(socket, token, 31*time.Minute)
	if err != nil {
		return failInitialization(err)
	}
	deferral, err := releaseeligibility.NewFileStore(filepath.Join(stateRoot, "deferral.json"))
	if err != nil {
		return failInitialization(err)
	}
	// Native declarations and recovery helpers can outlive a committed update.
	// Their environment is a launch snapshot, not the installed version owner.
	installed, err := hostinstall.LoadEnrolledOwner(uid)
	installedSource := installed.Source
	if err != nil {
		if !runtimeCausesOnly(err, os.ErrNotExist) {
			return failInitialization(runtimeFailure("reading committed installation failed", err))
		}
		installedSource, _, err = hostinstall.PendingInstalledSource(uid, binary)
		if err != nil {
			return failInitialization(runtimeFailure("reading pending installation failed", err))
		}
	}
	if err := installedSource.Validate(); err != nil {
		return failInitialization(runtimeFailure("installed source identity is invalid", err))
	}
	loadInstalledSource := func() (installsource.Source, error) {
		source, committed, err := hostinstall.PendingInstalledSource(uid, binary)
		if err == nil && !committed {
			return installsource.Source{}, updated.ErrInstallationPending
		}
		return source, err
	}
	active, pending, err := updated.UnixActivationActive(stateRoot, binary, loadInstalledSource)
	if err == nil && !pending {
		installedSource, _, err = hostinstall.PendingInstalledSource(uid, binary)
		if err == nil {
			active, err = workerupdate.InstalledBaseline(installedSource, binary)
		}
	}
	if err != nil {
		return failInitialization(err)
	}
	if !pending {
		go healMachineGuard(ctx, stateRoot, installedSource, binary, stderr)
	}
	environment := map[string]string{}
	refreshManuals, err := newUnixManualRefresher(binary, uid, gid)
	if err != nil {
		return failInitialization(err)
	}
	for _, key := range []string{"PAPERBOAT_UPDATE_STATE_ROOT", "PAPERBOAT_BINARY", "PAPERBOAT_BINARY_ROLLBACK", "PAPERBOAT_BINARY_STAGED", "PAPERBOAT_RELEASE_ROOT", "PAPERBOAT_HOSTD_SOCKET", "PAPERBOAT_HOSTD_TOKEN_FILE", "PAPERBOAT_RELEASE_REPOSITORY", "PAPERBOAT_MACHINE_ID", "PAPERBOAT_UPDATE_HEALTH_URL", "PAPERBOAT_ENROLLED_UID", "PAPERBOAT_ENROLLED_GID", "PAPERBOAT_UPDATED_SOCKET", "PAPERBOAT_INSTALL_SOURCE"} {
		if value, ok := os.LookupEnv(key); ok {
			environment[key] = value
		}
	}
	recoveryExecutable := ""
	if *helper {
		recoveryExecutable = filepath.Join(stateRoot, "activation", "pb")
	}
	applyPrepared := func(applyCtx context.Context, candidate workerupdate.PreparedActivation) (installsource.Source, error) {
		hooks := hostinstall.PreparedUpdateHooks{DrainTimeout: candidate.Signed.DrainTimeout, Prepare: candidate.Prepare, Validate: candidate.Validate, AuthorizeRestore: func(restoreCtx context.Context, previous installsource.Source, executable string) error {
			if err := previous.Verify(executable); err != nil {
				return err
			}
			if previous.Distribution == installsource.Custom {
				return nil
			}
			source := workerupdate.TUFSource{RepositoryURL: repository, StateRoot: filepath.Join(stateRoot, "tuf"), MachineID: machineID, FailureDomain: workerupdate.HostdFailureDomainSource{Client: hostdClient, MachineID: machineID}, Deferral: deferral}
			return source.AuthorizeRecovery(restoreCtx, previous.Version, previous.Platform, previous.Architecture)
		}}
		if err := hostinstall.ActivatePreparedUpdate(applyCtx, uid, candidate.Executable, candidate.Source, candidate.Force, hooks); err != nil {
			return installsource.Source{}, err
		}
		committed, err := hostinstall.LoadEnrolledOwner(uid)
		if err != nil {
			return installsource.Source{}, err
		}
		return committed.Source, nil
	}
	updaterService, err := updated.New(updated.Config{FinalizePrepared: func(ctx context.Context, _ installsource.Source) error {
		return hostinstall.FinalizePreparedUpdate(ctx, uid)
	}, NativeRecoveryStarted: func(ctx context.Context, previous, candidate workerupdate.Release) (bool, error) {
		previousSource := installsource.Source{Version: previous.Version, Platform: previous.Platform, Architecture: previous.Architecture, SHA256: previous.SHA256, Length: previous.Length, Distribution: installsource.Official, AutomaticUpdates: installedSource.AutomaticUpdates}
		if previous.LocalSource != nil {
			previousSource = *previous.LocalSource
		}
		candidateSource := installsource.Source{Version: candidate.Version, Platform: candidate.Platform, Architecture: candidate.Architecture, SHA256: candidate.SHA256, Length: candidate.Length, Distribution: installsource.Official, AutomaticUpdates: previousSource.AutomaticUpdates}
		return hostinstall.PreparedUpdateRecoveryStarted(ctx, uid, previousSource, candidateSource)
	}, LoadInstalledSource: loadInstalledSource, ApplyPrepared: applyPrepared, RecoveryExecutable: recoveryExecutable, AutomaticUpdates: installedSource.AutomaticUpdates, StateRoot: stateRoot, Binary: binary, BinaryRollback: binaryRollback, BinaryStaged: binaryStaged, Active: active, WorkerUID: uid, WorkerGID: gid, SocketPath: socket, Token: token, RepositoryURL: repository, MachineID: machineID, Health: updated.HTTPHealth{Endpoint: healthURL}, ControlSocket: controlSocket, Participants: participants, ActivationController: service.UnixUpdateActivator{Platform: runtime.GOOS, UID: uid, Runner: service.ExecRunner{}}, Environment: environment, RefreshManuals: refreshManuals, ReconcileCarrier: func(ownerCtx context.Context) error {
		return hostinstall.ReconcileInstalledCarrier(ownerCtx, "u"+strconv.Itoa(uid))
	}, SessionOwners: func(ownerCtx context.Context) (sessionowner.Coordinator, error) {
		return hostinstall.InstalledSessionOwnerCoordinator(ownerCtx, uid)
	}, PriorOwnerRecorded: func(ownerCtx context.Context, release workerupdate.Release) (bool, error) {
		return hostinstall.InstalledSessionOwnerRecorded(ownerCtx, uid, installsource.Source{Version: release.Version, Platform: release.Platform, Architecture: release.Architecture, SHA256: release.SHA256, Length: release.Length, Distribution: installsource.Official})

	}})
	if err != nil {
		return failInitialization(err)
	}
	if *helper {
		return updaterService.RunActivationHelper(ctx)
	}
	return runNotifiedUpdater(ctx, updaterRunnerFunc(func(ctx context.Context, ready func() error) error {
		return updaterService.RunWithReady(ctx, ready)
	}), notifier)
}

type updaterRunner interface {
	Run(context.Context, func() error) error
}

type updaterRunnerFunc func(context.Context, func() error) error

func (f updaterRunnerFunc) Run(ctx context.Context, ready func() error) error {
	return f(ctx, ready)
}

var errUpdaterExitedBeforeReadiness = errors.New("paperboat-updated exited before readiness")

type processNotifier interface {
	Ready() error
	Degraded(string) error
	Draining() error
	Stopping() error
	WatchdogInterval() time.Duration
	Watchdog() error
}

// runNotifiedUpdater keeps the systemd notify contract aligned with the
// updater service declaration. The scheduler is run in a child context so a
// failed watchdog notification can stop it before returning the failure. The
// runner invokes ready after its authenticated listener is accepting work.
func runNotifiedUpdater(ctx context.Context, runner updaterRunner, notifier processNotifier) error {
	if ctx == nil || runner == nil || notifier == nil {
		return errors.New("invalid updater lifecycle")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	readyDone := make(chan error, 1)
	go func() {
		done <- runner.Run(runCtx, func() error {
			err := notifier.Ready()
			readyDone <- err
			return err
		})
	}()
	for {
		select {
		case readyErr := <-readyDone:
			if readyErr != nil {
				cancel()
				runErr := <-done
				degradedErr := notifier.Degraded("updater readiness notification failed")
				stoppingErr := notifier.Stopping()
				return errors.Join(readyErr, degradedErr, runErr, stoppingErr)
			}
			goto running
		case runErr := <-done:
			// A runner that returns after invoking a successful ready callback
			// may win this select before the callback signal is consumed.
			select {
			case readyErr := <-readyDone:
				if readyErr == nil {
					if ctx.Err() != nil {
						return updaterShutdownResult(ctx, runErr, notifier.Stopping())
					}
					return errors.Join(runErr, notifier.Stopping())
				}
				degradedErr := notifier.Degraded("updater readiness notification failed")
				stoppingErr := notifier.Stopping()
				return errors.Join(runErr, readyErr, degradedErr, stoppingErr)
			default:
				if ctx.Err() != nil {
					// Stopped by the service manager before it became ready: a requested stop, not a failure.
					return updaterShutdownResult(ctx, runErr, notifier.Stopping())
				}
				degradedErr := notifier.Degraded("updater exited before readiness")
				stoppingErr := notifier.Stopping()
				return errors.Join(errUpdaterExitedBeforeReadiness, runErr, degradedErr, stoppingErr)
			}
		case <-ctx.Done():
			drainErr := notifier.Draining()
			cancel()
			runErr := <-done
			return updaterShutdownResult(ctx, drainErr, runErr, notifier.Stopping())
		}
	}

running:

	interval := notifier.WatchdogInterval()
	var watchdog <-chan time.Time
	var ticker *time.Ticker
	if interval > 0 {
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
		watchdog = ticker.C
	}
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				// The stop request cancels the runner too, so its return can win the select against ctx.Done.
				return updaterShutdownResult(ctx, notifier.Draining(), err, notifier.Stopping())
			}
			return errors.Join(err, notifier.Stopping())
		case <-ctx.Done():
			drainErr := notifier.Draining()
			cancel()
			runErr := <-done
			return updaterShutdownResult(ctx, drainErr, runErr, notifier.Stopping())
		case <-watchdog:
			if err := notifier.Watchdog(); err != nil {
				degradedErr := notifier.Degraded("updater watchdog notification failed")
				drainErr := notifier.Draining()
				cancel()
				runErr := <-done
				stoppingErr := notifier.Stopping()
				return errors.Join(err, degradedErr, drainErr, runErr, stoppingErr)
			}
		}
	}
}

// updaterShutdownResult reports a stop that the service manager requested as a clean exit. systemd
// stops the old updater during every update; counting the cancellation itself as a failure exited the
// unit with status 1 and made the activation fail with drain_failed. Any other shutdown cause fails.
func updaterShutdownResult(ctx context.Context, causes ...error) error {
	failures := []error{withoutCancellation(runtimeContextFailure(ctx))}
	for _, cause := range causes {
		failures = append(failures, withoutCancellation(cause))
	}
	return runtimeFailureIfAny("updater shutdown failed", failures...)
}

func withoutCancellation(err error) error {
	if err != nil && runtimeCausesOnly(err, context.Canceled) {
		return nil
	}
	return err
}

func validRuntimeIdentity(uid, gid int) bool {
	return uid > 0 && gid > 0 || uid == 0 && gid == 0
}
