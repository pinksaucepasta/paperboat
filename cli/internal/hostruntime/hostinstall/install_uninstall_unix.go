//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

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
	if err == nil && request.Source.Verify(paths.worker) == nil {
		return paths.worker, nil
	}
	// No usable record: an earlier reset removed it, or a half-install (failed
	// pair or update) left one that is unreadable or names another build. The
	// protected slot's own Paperboat identity decides, as when it is absent.
	if _, err := installsource.Inspect(paths.worker, "reset", installsource.Custom); err != nil {
		return "", err
	}
	return paths.worker, nil
}

type resetCLIContextKey struct{}

// UninstallPersistedPreservingCLI keeps only keep, the CLI that
// ResetCLIForCurrentAccount proved.
func UninstallPersistedPreservingCLI(ctx context.Context, keep string) error {
	return UninstallPersisted(context.WithValue(ctx, resetCLIContextKey{}, keep))
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
	legacyHost, err = hostInstallerWithMissing(request, paths, true)
	if err != nil {
		return errors.Join(ErrInvalidRequest, err)
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
		if err := restoreUserLinger(ctx, request, paths); err != nil {
			return withUninstallStage("uninstall_linger", err)
		}
		return withUninstallStage("uninstall_files", errors.Join(removeCommandLinkUnlessKept(ctx, request, paths), removeInstalledFiles(ctx, paths)))
	}
	hostd, updater, err := pendingInstallers(request, paths)
	if err != nil {
		return withUninstallStage("uninstall_prepare", errors.Join(ErrInvalidRequest, err))
	}
	var legacyHost *service.Installer
	legacyHost, err = hostInstallerWithMissing(request, paths, true)
	if err != nil {
		return withUninstallStage("uninstall_prepare", errors.Join(ErrInvalidRequest, err))
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
		if err := uninstallWithActivation(ctx, controller.RemoveForUninstall, lifecycle.Uninstall); err != nil {
			return err
		}
		// Generations still retiring after an update are not lifecycle components.
		return withUninstallStage("uninstall_generations", sweepGenerations(ctx, request, "", true))
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
	if err := restoreUserLinger(ctx, request, paths); err != nil {
		return withUninstallStage("uninstall_linger", err)
	}
	// A reset retains its CLI slot (ResetCLIForCurrentAccount), so the account keeps the link to it: `pb pair` follows.
	linkErr := removeCommandLinkUnlessKept(ctx, request, paths)
	journal, journalErr := loadJournal(paths.journal)
	if journalErr == nil {
		return withUninstallStage("uninstall_files", errors.Join(linkErr, rollbackFiles(paths, journal)))
	}
	if !installNotExistOnly(journalErr) {
		return withUninstallStage("uninstall_journal", errors.Join(linkErr, journalErr))
	}
	return withUninstallStage("uninstall_files", errors.Join(linkErr, removeInstalledFiles(ctx, paths)))
}

// removeCommandLinkUnlessKept leaves the link when a reset retains this installation's CLI slot
// (ResetCLIForCurrentAccount): the account keeps the link to it, so `pb pair` follows.
func removeCommandLinkUnlessKept(ctx context.Context, request Request, paths installPaths) error {
	if keep, _ := ctx.Value(resetCLIContextKey{}).(string); keep == paths.worker {
		return nil
	}
	return removeCommandLink(request, paths)
}

// removeCommandLink removes the ~/.local/bin/pb link the install made, so the account is not left with a `pb` that
// points at the system copy this uninstall removes ("command not found"). The user-level cleanup also removes it, but
// only after the service stopped; the link is this installation's, whatever that cleanup decides. A link to anything
// else, or a regular file, is the account's own and stays.
func removeCommandLink(request Request, paths installPaths) error {
	if !filepath.IsAbs(request.Home) || paths.worker == "" {
		return nil
	}
	link := filepath.Join(request.Home, ".local", "bin", "pb")
	target, err := os.Readlink(link)
	if err != nil || target != paths.worker {
		return nil // absent, not a link, or not ours
	}
	if err := os.Remove(link); err != nil && !installNotExistOnly(err) {
		return err
	}
	return nil
}

func uninstallWithActivation(ctx context.Context, retire, uninstall func(context.Context) error) error {
	if err := retire(ctx); err != nil {
		return withUninstallStage("uninstall_activator", err)
	}
	return withUninstallStage("uninstall_services", uninstall(ctx))
}
