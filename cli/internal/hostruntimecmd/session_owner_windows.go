//go:build windows

package hostruntimecmd

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"io"
	"path/filepath"
)

func runSessionOwner(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) error {
	generation, instance, err := sessionOwnerInvocation(args, stderr)
	if err != nil {
		return err
	}
	root, err := hostinstall.WindowsInstanceRoot(instance)
	if err != nil {
		return err
	}
	catalog := filepath.Join(root, "session-owners")
	d, err := readSessionOwnerDescriptor(catalog, generation)
	if err != nil {
		return err
	}
	if d.Instance != instance {
		return sessionowner.ErrInvalid
	}
	account, err := hostinstall.WindowsInstanceForSID(d.OwnerSID)
	if err != nil {
		return err
	}
	if account != instance {
		return sessionowner.ErrInvalid
	}
	sid, err := currentWindowsSID()
	if err != nil {
		return err
	}
	if sid == d.OwnerSID {
		// The SCM parent owns the enrolled user's job; IPC readiness is checked
		// by the catalog before hostd can use this generation.
		return runIndependentSessions(ctx, d, catalog, func() error { return nil }, func() error { return nil }, 0)
	}
	if sid != "S-1-5-18" {
		return sessionowner.ErrInvalid
	}
	// The native declaration is SYSTEM/Admin-only. The privileged parent
	// verifies it before selecting the enrolled token; the child proves its
	// protected descriptor, actual executable digest, account and store lock.
	if _, err := service.VerifyOwnedWindowsSessionOwnerExecutable(instance, generation, d.Executable); err != nil {
		return err
	}
	name, err := service.WindowsSessionOwnerName(instance, generation)
	if err != nil {
		return err
	}
	return service.RunWindowsService(service.ServiceEntryConfig{
		Name: name, Executable: d.Executable, EnrolledSID: d.OwnerSID,
		Arguments:   []string{"daemon", "__runtime-session-owner", "--generation", generation, "--instance", instance},
		Environment: map[string]string{"PAPERBOAT_SESSION_OWNER_CATALOG": catalog, "PAPERBOAT_RUNTIME_STATE_ROOT": d.RuntimeRoot, "PAPERBOAT_WORKSPACE_ROOT": d.Workspace, "PAPERBOAT_SHELL": d.Shell},
	})
}
