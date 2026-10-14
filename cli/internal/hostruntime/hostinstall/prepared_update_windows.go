//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// ActivatePreparedWindowsUpdate installs the original signed candidate through
// the same native lifecycle owner used for supplied installations. Composition
// owns signed approval/recovery policy; state must reauthorize restoration, and
// identity is the exact trusted baseline captured by that policy owner.
func ActivatePreparedWindowsUpdate(ctx context.Context, ownerSID, executable string, source installsource.Source, force bool, hooks PreparedUpdateHooks, state service.NativeInstallationState, identity WindowsRollbackIdentity) (resultErr error) {
	if ctx == nil || state == nil || hooks.Prepare == nil || hooks.Validate == nil || hooks.AuthorizeRestore == nil || hooks.DrainTimeout <= 0 || hooks.DrainTimeout > 2*time.Minute || !validSID(ownerSID) || !safeAbsolute(executable) || source.Validate() != nil || source.Distribution != installsource.Official || source.Platform != "windows" || source.Architecture != runtime.GOARCH || identity.Source.Validate() != nil || identity.Source.Platform != "windows" || identity.Source.Architecture != runtime.GOARCH {
		return ErrInvalidRequest
	}
	allowed, err := checkAdministrator()
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotPrivileged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := source.Verify(executable); err != nil {
		return err
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return err
	}
	unlock, err := LockWindowsNativePublication(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	// A crash can publish the committed candidate before the lifecycle receipt
	// disappears. Recover its authenticated captured baseline before projection.
	if err := recoverWindowsInstallationLocked(ctx, ownerSID, state); err != nil {
		return err
	}
	previous, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return err
	}
	if previous.OwnerSID != ownerSID || previous.EnrollmentPending || !previous.Committed {
		return ErrInvalidRequest
	}
	source.AutomaticUpdates = previous.Source.AutomaticUpdates
	if previous.Source == source {
		if err := source.Verify(layout.Binary); err != nil {
			return err
		}
		return hooks.Validate(ctx)
	}
	if previous.Source != identity.Source {
		return ErrInvalidRequest
	}
	request := Request{
		Schema: SchemaV1, Platform: "windows", OwnerSID: ownerSID, User: previous.User,
		Executable: executable, Source: source, Artifact: previous.Artifact,
		SessionOwnerGeneration: previous.SessionOwnerGeneration,
		StateRoot:              previous.StateRoot, WorkspaceRoot: previous.Workspace,
		Home: filepath.Dir(previous.StateRoot), ControlURL: previous.ControlURL,
		UserMachineID: previous.MachineID, HelperListenAddress: previous.ListenAddress,
		Force: force, RollbackIdentity: &identity,
	}
	request.Artifact.Version = source.Version
	if err := Validate(request, 0); err != nil {
		return err
	}
	return replaceWindowsInstallationLocked(ctx, request, state, func(ctx context.Context) error {
		if err := InstallWindowsReplacement(ctx, request); err != nil {
			return err
		}
		// Native role startup needs its candidate configuration. Committed remains
		// false under the owning lifecycle journal until signed health has passed.
		if err := hooks.Validate(ctx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		installed, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
		if err != nil {
			return err
		}
		if installed.OwnerSID != ownerSID || installed.Source != source || installed.Committed {
			return ErrInvalidRequest
		}
		if err := source.Verify(layout.Binary); err != nil {
			return err
		}
		if err := Commit(request); err != nil {
			return err
		}
		return EnsureCommittedWindowsHostdReady(ctx, request)
	}, hooks)
}
