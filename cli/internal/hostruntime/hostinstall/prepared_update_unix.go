//go:build darwin || linux

package hostinstall

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

type unixInstallIntent uint8

const (
	unixEnrollmentInstall unixInstallIntent = iota
	unixSuppliedInstall
	unixPreparedUpdate
)

// PreparedUpdateRecoveryStarted distinguishes a native replacement receipt or
// committed candidate from an untouched approval. It only reads protected state;
// the caller still rechecks current signed recovery policy before any mutation.
func PreparedUpdateRecoveryStarted(ctx context.Context, uid int, previous, candidate installsource.Source) (bool, error) {
	if os.Geteuid() != 0 {
		return false, ErrNotPrivileged
	}
	if ctx == nil || uid < 0 || previous.Validate() != nil || candidate.Validate() != nil || candidate.Distribution != installsource.Official || previous.Platform != runtime.GOOS || candidate.Platform != runtime.GOOS || previous.Architecture != runtime.GOARCH || candidate.Architecture != runtime.GOARCH {
		return false, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	paths := platformPaths(uid)
	return preparedUpdateRecoveryStarted(ctx, paths, uid, previous, candidate)
}

func preparedUpdateRecoveryStarted(ctx context.Context, paths installPaths, uid int, previous, candidate installsource.Source) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	source, committed, err := pendingInstalledSource(paths, uid)
	if err != nil {
		return false, err
	}
	if committed && source == candidate {
		return true, nil
	}
	installed, err := loadInstallMetadata(paths.metadata, uid)
	if err != nil {
		return false, err
	}
	body, err := pendingSourceBaseline(ctx, paths, installed)
	if err != nil || len(body) == 0 {
		return false, err
	}
	return preparedUpdateBaselineMatches(body, uid, installed, previous)
}

func preparedUpdateBaselineMatches(body []byte, uid int, installed Request, previous installsource.Source) (bool, error) {
	var baseline unixCarrierBaseline
	if err := decodeInstallJSON(bytes.NewReader(body), &baseline, 256<<10); err != nil {
		return false, err
	}
	if baseline.CarrierOnly || baseline.Worker == nil || *baseline.Worker != previous {
		return false, ErrInvalidRequest
	}
	var before Request
	if err := decodeInstallJSON(bytes.NewReader(baseline.Metadata), &before, 128<<10); err != nil {
		return false, err
	}
	if before.Schema != SchemaV1 || before.Source != previous || before.UID != uid || before.Platform != installed.Platform || before.GID != installed.GID || before.User != installed.User || before.Group != installed.Group || before.Home != installed.Home || before.StateRoot != installed.StateRoot || before.UserMachineID != installed.UserMachineID || before.InstallationGeneration != installed.InstallationGeneration {
		return false, ErrInvalidRequest
	}
	return true, nil
}

// ActivatePreparedUpdate is called by the privileged activation helper while it
// holds the updater activation lock. The composition root verifies the original
// signed release and approved candidate; this boundary rechecks payload identity.
func ActivatePreparedUpdate(ctx context.Context, uid int, executable string, source installsource.Source, force bool, hooks PreparedUpdateHooks) (resultErr error) {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if ctx == nil || uid < 0 || hooks.Prepare == nil || hooks.Validate == nil || hooks.AuthorizeRestore == nil || hooks.DrainTimeout <= 0 || hooks.DrainTimeout > 2*time.Minute || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || source.Validate() != nil || source.Distribution != installsource.Official || source.Platform != runtime.GOOS || source.Architecture != runtime.GOARCH {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := source.Verify(executable); err != nil {
		return err
	}
	paths := platformPaths(uid)
	lock, err := LockSuppliedOperation(paths.metadata+".lock", 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("prepared update cleanup failed", resultErr, lock.Close()) }()
	previous, err := loadInstallMetadata(paths.metadata, uid)
	if err != nil {
		return err
	}
	if previous.EnrollmentPending {
		return ErrInvalidRequest
	}
	// A crash may publish metadata before the lifecycle receipt is removed.
	// Restore its protected baseline before deciding whether this is a retry.
	hostd, updater, err := pendingInstallers(previous, paths)
	if err != nil {
		return err
	}
	var host *service.Installer
	if runtime.GOOS != "darwin" {
		host, err = hostInstallerWithMissing(previous, paths, true)
		if err != nil {
			return err
		}
	}
	lifecycle, err := newLifecycleManagerWithRestorePolicy(previous, paths, host, hostd, updater, false, hooks.AuthorizeRestore)
	if err != nil {
		return err
	}
	if err = lifecycle.Recover(ctx); err != nil {
		return err
	}
	previous, err = loadInstallMetadata(paths.metadata, uid)
	if err != nil {
		return err
	}
	if previous.EnrollmentPending {
		return ErrInvalidRequest
	}
	if err = previous.Source.Verify(paths.worker); err != nil {
		return err
	}
	// Supplied installation recovery owns a different staging receipt. Never
	// consume it while a signed candidate and its handoff own activation.
	if _, err = os.Lstat(paths.journal); err == nil {
		return ErrInvalidRequest
	} else if !installNotExistOnly(err) {
		return err
	}
	if _, err = os.Lstat(filepath.Join(paths.updateState, "native-install.json")); err == nil {
		return ErrInvalidRequest
	} else if !installNotExistOnly(err) {
		return err
	}
	source.AutomaticUpdates = previous.Source.AutomaticUpdates
	if previous.Source == source {
		if err := source.Verify(paths.worker); err != nil {
			return err
		}
		if err := hooks.Validate(ctx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return source.Verify(paths.worker)
	}
	request := previous
	request.Executable, request.Source = executable, source
	request.Artifact.Version = source.Version
	err = installWithIntent(ctx, request, true, unixPreparedUpdate, &force, hooks)
	if err != nil {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		pendingErr := lifecycle.RequireRecovered(recoveryCtx)
		cancel()
		if pendingErr != nil {
			return errors.Join(err, service.ErrLifecycleUncertain, pendingErr)
		}
	}
	return err
}

// This runs inside lifecycle.Replace: rejection leaves the old metadata untouched
// and the lifecycle restores the exact verified source and native role baseline.
func commitValidatedPreparedUpdate(ctx context.Context, request Request, paths installPaths, validate func(context.Context) error) error {
	if err := validate(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := request.Source.Verify(paths.worker); err != nil {
		return err
	}
	request.Executable = paths.worker
	request.SessionOwnerGeneration = request.Source.SHA256[:32]
	return writeInstallMetadata(paths.metadata, request)
}

// Native recovery must recheck current recovery policy, not merely a historical
// checksum. Sources come exclusively from the protected lifecycle baseline.
type preparedUnixInstallation struct {
	state     service.NativeInstallationState
	paths     installPaths
	authorize func(context.Context, installsource.Source, string) error
}

func (s preparedUnixInstallation) Capture(ctx context.Context) ([]byte, error) {
	return s.state.Capture(ctx)
}
func (s preparedUnixInstallation) Restore(ctx context.Context, body []byte) error {
	var baseline unixCarrierBaseline
	if err := decodeInstallJSON(bytes.NewReader(body), &baseline, 256<<10); err != nil {
		return err
	}
	if baseline.Worker == nil {
		return ErrInvalidRequest
	}
	source := *baseline.Worker
	pin := filepath.Join(filepath.Dir(s.paths.workerRollback), "pins", source.SHA256, "pb")
	if err := source.Verify(pin); err != nil {
		return err
	}
	if err := s.authorize(ctx, source, pin); err != nil {
		return err
	}
	if baseline.Carrier != nil {
		carrier := baseline.Carrier
		if carrier.Validate() != nil {
			return ErrInvalidRequest
		}
		if err := carrier.Source.Verify(carrier.Executable); err != nil {
			return err
		}
		if err := s.authorize(ctx, carrier.Source, carrier.Executable); err != nil {
			return err
		}
	}
	return s.state.Restore(ctx, body)
}

// The caller holds the activation lock. Only the signed recovery helper may
// execute a pending native replacement baseline under current recovery policy.
func prepareUnixNativeRecovery(ctx context.Context, paths installPaths, uid int, supplied bool) error {
	if _, err := os.Lstat(paths.updateState); installNotExistOnly(err) {
		return nil
	} else if err != nil {
		return err
	}
	if supplied {
		previous, err := LoadEnrolledOwner(uid)
		if err != nil {
			return err
		}
		controller := service.UnixUpdateActivator{Platform: previous.Platform, UID: uid, Runner: service.ExecRunner{}}
		if err = updated.SupersedeUntouchedUnixActivation(ctx, paths.updateState, paths.worker, previous.Source, controller); err != nil {
			return err
		}
	}
	_, active, err := updated.UnixActivationActive(paths.updateState, "", nil)
	if err != nil {
		return err
	}
	if active {
		return updated.ErrActivationPending
	}
	return nil
}

// FinalizePreparedUpdate publishes the committed declaration without a service
// restart. Its caller owns the activation lock and has committed the signed gate.
func FinalizePreparedUpdate(ctx context.Context, uid int) (resultErr error) {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if ctx == nil || uid < 0 {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	paths := platformPaths(uid)
	lock, err := LockSuppliedOperation(paths.metadata+".lock", 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("prepared finalization cleanup failed", resultErr, lock.Close()) }()
	return finalizePreparedUpdateLocked(ctx, uid)
}

func finalizePreparedUpdateLocked(ctx context.Context, uid int) error {
	paths := platformPaths(uid)
	request, err := loadInstallMetadata(paths.metadata, uid)
	if err != nil {
		return err
	}
	if request.EnrollmentPending {
		return ErrInvalidRequest
	}
	if err = request.Source.Verify(paths.worker); err != nil {
		return err
	}
	hostd, updater, err := installers(request, paths)
	if err != nil {
		return err
	}
	var host *service.Installer
	if runtime.GOOS != "darwin" {
		host, err = hostInstallerWithMissing(request, paths, true)
		if err != nil {
			return err
		}
	}
	lifecycle, err := newLifecycleManager(request, paths, host, hostd, updater)
	if err != nil {
		return err
	}
	if err = lifecycle.RequireRecovered(ctx); err != nil {
		return err
	}
	if err = hostd.PublishDefinition(ctx); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		return (service.ExecRunner{}).Run(ctx, "systemctl", "daemon-reload")
	}
	return nil
}
