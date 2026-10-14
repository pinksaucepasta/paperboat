//go:build windows

package hostinstall

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"golang.org/x/sys/windows"
	"path/filepath"
	"runtime"
)

func sessionOwnerCatalog(request Request) (sessionowner.Catalog, string, error) {
	instance, err := WindowsInstanceForSID(request.OwnerSID)
	if err != nil {
		return sessionowner.Catalog{}, "", err
	}
	root, err := WindowsInstanceRoot(instance)
	if err != nil {
		return sessionowner.Catalog{}, "", err
	}
	return sessionowner.Catalog{Root: filepath.Join(root, "session-owners"), ConfigRoot: root, OwnerSID: request.OwnerSID, User: "Paperboat", Group: "Paperboat", Maximum: config.DefaultResources.MaxSessions + 1, Runner: service.ExecRunner{}}, instance, nil
}
func NativeSessionOwnerConfig(ctx context.Context, request Request) (sessionowner.Catalog, sessionowner.Descriptor, error) {
	if ctx == nil {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, ErrInvalidRequest
	}
	allowed, err := checkAdministrator()
	if err != nil {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, err
	}
	if !allowed {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, ErrNotPrivileged
	}
	if err := ctx.Err(); err != nil {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, err
	}
	if request.Source.Validate() != nil || request.Source.Platform != "windows" || request.Source.Architecture != runtime.GOARCH {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, ErrInvalidRequest
	}
	c, instance, err := sessionOwnerCatalog(request)
	if err != nil {
		return c, sessionowner.Descriptor{}, err
	}
	layout, err := service.WindowsUserLayout(request.OwnerSID)
	if err != nil {
		return c, sessionowner.Descriptor{}, err
	}
	pin, err := ensureWindowsPinnedRuntimeFrom(ctx, layout, request.Source, request.OwnerSID, request.Executable)
	if err != nil {
		return c, sessionowner.Descriptor{}, err
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return c, sessionowner.Descriptor{}, err
	}
	generation := request.Source.SHA256[:32]
	d := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: generation, Instance: instance, Source: request.Source, Executable: pin, RuntimeRoot: request.StateRoot, Endpoint: `\\.\pipe\PaperboatSessionOwner-` + instance + "-" + generation, UID: request.UID, GID: request.GID, OwnerSID: request.OwnerSID, Workspace: request.WorkspaceRoot, Shell: filepath.Join(system, "cmd.exe")}
	d, err = bindInitialSessionStore(ctx, c, d, request.SessionOwnerGeneration)
	if err != nil {
		return c, d, err
	}
	return c, d, d.Validate()
}
func PrepareSessionOwner(ctx context.Context, request Request) error {
	c, d, err := NativeSessionOwnerConfig(ctx, request)
	if err != nil {
		return err
	}
	return startPreparedSessionOwner(ctx, c, d)
}
func prepareSessionOwner(ctx context.Context, request Request, _ service.Layout) error {
	return PrepareSessionOwner(ctx, request)
}

func InstalledSessionOwnerCoordinator(ctx context.Context, instance string) (sessionowner.Coordinator, error) {
	cfg, err := LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return sessionowner.Coordinator{}, err
	}
	layout, err := service.WindowsUserLayout(cfg.OwnerSID)
	if err != nil {
		return sessionowner.Coordinator{}, err
	}
	request := Request{Platform: "windows", OwnerSID: cfg.OwnerSID, User: cfg.User, StateRoot: cfg.StateRoot, WorkspaceRoot: cfg.Workspace, Executable: layout.Binary, Source: cfg.Source, SessionOwnerGeneration: cfg.SessionOwnerGeneration}
	if err := cfg.Source.Verify(layout.Binary); err != nil {
		return sessionowner.Coordinator{}, err
	}
	recorded, err := SessionOwnerRecorded(ctx, request)
	if err != nil {
		return sessionowner.Coordinator{}, err
	}
	if !recorded {
		return sessionowner.Coordinator{}, sessionowner.ErrInvalid
	}
	c, d, err := NativeSessionOwnerConfig(ctx, request)
	return sessionowner.Coordinator{Catalog: c, Active: d}, err
}
