//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
)

// WindowsPinnedRuntimePath is the sole immutable runtime image shape used by
// native SCM owners and replacement feature workers.
func WindowsPinnedRuntimePath(layout service.Layout, version string) (string, error) {
	if version == "" || filepath.Base(version) != version || strings.ContainsAny(version, "/\\\x00\r\n") || version == "." || version == ".." {
		return "", ErrInvalidRequest
	}
	return filepath.Join(layout.ReleasesRoot, "versions", version, "pb.exe"), nil
}

// WindowsPinnedSourcePath binds custom image identity to bytes, allowing
// separately approved builds to share a display version without slot collision.
func WindowsPinnedSourcePath(layout service.Layout, source installsource.Source) (string, error) {
	if err := source.Validate(); err != nil {
		return "", err
	}
	key := source.Version
	if source.Distribution == installsource.Custom {
		key = "custom-" + source.SHA256
	}
	return WindowsPinnedRuntimePath(layout, key)
}

func ensureWindowsPinnedRuntime(ctx context.Context, layout service.Layout, source installsource.Source, ownerSID string) (string, error) {
	return ensureWindowsPinnedRuntimeFrom(ctx, layout, source, ownerSID, layout.Binary)
}
func ensureWindowsPinnedRuntimeFrom(ctx context.Context, layout service.Layout, source installsource.Source, ownerSID, executable string) (string, error) {
	if err := prepareWindowsAccountPinRoot(ctx, layout, ownerSID); err != nil {
		return "", err
	}
	for _, parent := range []string{layout.ReleasesRoot, filepath.Join(layout.ReleasesRoot, "versions")} {
		if err := protectWindowsPinDirectory(parent, ownerSID); err != nil {
			return "", err
		}
	}
	path, err := WindowsPinnedSourcePath(layout, source)
	if err != nil {
		return "", err
	}
	if _, err = os.Lstat(path); err == nil {
		if err = verifyWindowsSource(ctx, path, source, ownerSID); err != nil {
			installed, bindingErr := LoadWindowsRuntimeConfigForInstance(layout.Instance)
			if bindingErr != nil || installed.OwnerSID != ownerSID || !installed.Source.SameArtifact(source) || installed.Source.Distribution != installsource.Official {
				return "", errors.Join(err, bindingErr, ErrInvalidRequest)
			}
			if repairErr := protectVerifiedWindowsReleasePin(ctx, path, source, ownerSID); repairErr != nil {
				return "", errors.Join(err, repairErr)
			}
			if err := protectWindowsPinDirectory(filepath.Dir(path), ownerSID); err != nil {
				return "", err
			}
			if err := verifyWindowsSource(ctx, path, source, ownerSID); err != nil {
				return "", err
			}
		}
		return path, nil
	} else if !installNotExistOnly(err) {
		return "", err
	}
	if err = stageWindowsBinary(ctx, executable, path, path+".rollback", source, ownerSID, nil); err != nil {
		return "", err
	}
	return path, nil
}
func installedWindowsPinnedRuntime(layout service.Layout) (string, error) {
	config, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return "", err
	}
	return WindowsPinnedSourcePath(layout, config.Source)
}

// PublishWindowsRoleDefinition keeps the existing declaration authoritative
// while an update journal changes SCM targets under an already stopped owner.
func PublishWindowsRoleDefinition(ctx context.Context, ownerSID, kind, executable, digest string, length int64) error {
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return err
	}
	root, err := WindowsInstanceRoot(layout.Instance)
	if err != nil {
		return err
	}
	role := ""
	switch kind {
	case service.HostdKind:
		role = "__runtime-hostd"
	case service.UpdaterKind:
		role = "__runtime-updated"
	case service.DaemonKind:
		role = "__runtime-local-daemon"
	default:
		return ErrInvalidRequest
	}
	installer, err := service.New(service.Config{Platform: "windows", Kind: kind, Instance: layout.Instance, ConfigRoot: root, Executable: executable, ExecutableSHA256: digest, ExecutableLength: length, User: "Paperboat", Group: "Paperboat", Arguments: []string{"daemon", role, "--instance", layout.Instance}, Controller: service.WindowsController{}})
	if err != nil {
		return err
	}
	return installer.PublishDefinition(ctx)
}

func windowsPinnedRuntimeOrInvalid(layout service.Layout) string {
	path, _ := windowsRoleRuntime(layout, service.DaemonKind)
	return path
}

func windowsRoleRuntime(layout service.Layout, kind string) (string, error) {
	root, err := WindowsInstanceRoot(layout.Instance)
	if err != nil {
		return "", err
	}
	role := "__runtime-hostd"
	switch kind {
	case service.UpdaterKind:
		role = "__runtime-updated"
	case service.DaemonKind:
		role = "__runtime-local-daemon"
	}
	pending, err := service.NewPending(service.Config{Platform: "windows", Kind: kind, Instance: layout.Instance, ConfigRoot: root, Executable: layout.Binary, User: "Paperboat", Group: "Paperboat", Arguments: []string{"daemon", role, "--instance", layout.Instance}, Controller: service.WindowsController{}})
	if err != nil {
		return "", err
	}
	existing, err := service.OwnedWindowsExecutable(pending.DefinitionPath())
	if err == nil {
		relative, relativeErr := filepath.Rel(filepath.Join(layout.ReleasesRoot, "versions"), existing)
		parts := strings.Split(relative, string(filepath.Separator))
		pinned := relativeErr == nil && len(parts) == 2 && parts[0] != ".." && parts[0] != "." && parts[1] == "pb.exe"
		if !strings.EqualFold(existing, layout.Binary) && !pinned {
			return "", service.ErrInvalidDefinition
		}
		return existing, nil
	}
	if !installNotExistOnly(err) {
		return "", err
	}
	return installedWindowsPinnedRuntime(layout)
}

// Freeze the exact non-reparse directory before repairing its custody. The
// enrolled account cannot replace the handle during the SYSTEM owner transfer.
func protectWindowsPinDirectory(path, ownerSID string) (resultErr error) {
	if err := rejectWindowsReparseAncestors(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	handle, _, err := openWindowsRuntimeObject(path, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		return err
	}
	dacl := windowsRuntimeCurrentRootDACL(ownerSID)
	if err := applyWindowsHandleOwnedDACL(handle, owner, dacl); err != nil {
		return err
	}
	return checkWindowsRuntimeHandleSecurity(handle, owner, dacl)
}

// PrepareWindowsPinNamespace protects only the fixed per-account artifact
// parents. Private updater state keeps its separate SYSTEM-only policy.
func PrepareWindowsPinNamespace(ctx context.Context, ownerSID string) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return err
	}
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		return err
	}
	if err := validateWindowsRuntimeSecurity(layout.InstallRoot, owner, windowsRuntimeCurrentRootDACL(ownerSID), "pin account root"); err != nil {
		return err
	}
	for _, parent := range []string{layout.ReleasesRoot, filepath.Join(layout.ReleasesRoot, "versions")} {
		if err := protectWindowsPinDirectory(parent, ownerSID); err != nil {
			return err
		}
	}
	return nil
}

// LockWindowsNativePublication serializes immutable artifact+journal publication
// with native installation and pin pruning. It never spans user approval.
// The returned unlock must run in the acquiring goroutine (Windows mutexes
// own an OS thread, which remains pinned until release).
func LockWindowsNativePublication(ctx context.Context) (func() error, error) {
	return lockWindowsLocalDaemonMigration(ctx)
}
