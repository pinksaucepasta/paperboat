//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
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
func ensureWindowsPinnedRuntime(ctx context.Context, layout service.Layout, source installsource.Source, ownerSID string) (string, error) {
	path, err := WindowsPinnedRuntimePath(layout, source.Version)
	if err != nil {
		return "", err
	}
	if _, err = os.Lstat(path); err == nil {
		if err = source.Verify(path); err != nil {
			return "", err
		}
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err = stageWindowsBinary(ctx, layout.Binary, path, path+".rollback", source, ownerSID, nil); err != nil {
		return "", err
	}
	return path, nil
}
func installedWindowsPinnedRuntime(layout service.Layout) (string, error) {
	config, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return "", err
	}
	return WindowsPinnedRuntimePath(layout, config.Source.Version)
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
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return installedWindowsPinnedRuntime(layout)
}
