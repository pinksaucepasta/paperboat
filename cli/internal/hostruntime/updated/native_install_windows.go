//go:build windows

package updated

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// PrepareWindowsNativeInstall is called with the instance installation mutex
// held, after synchronously stopping the updater. It retires only completed
// transactions; the returned callback restores their policy state on failure,
// before the prior services restart. Signed metadata and release slots remain.
func PrepareWindowsNativeInstall(ctx context.Context, ownerSID string) (*hostinstall.WindowsRollbackIdentity, func() error, error) {
	noop := func() error { return nil }
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return nil, nil, err
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if errors.Is(err, os.ErrNotExist) {
		return nil, noop, nil
	}
	if err != nil {
		return nil, nil, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	defer manager.Disconnect()
	updater, err := manager.OpenService(windowsUpdaterService + "-" + layout.Instance)
	if err == nil {
		status, queryErr := updater.Query()
		updater.Close()
		if queryErr != nil {
			return nil, nil, queryErr
		}
		if status.State != svc.Stopped {
			return nil, nil, fmt.Errorf("stop the updater before reinstalling: %w", ErrWindowsActivationUnavailable)
		}
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil, err
	}
	activator, err := manager.OpenService(windowsActivatorService + "-" + layout.Instance)
	if err == nil {
		activator.Close()
		return nil, nil, fmt.Errorf("finish or recover the pending update before reinstalling: %w", ErrWindowsActivationUnavailable)
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil, err
	}
	updateConfig := WindowsConfig{OwnerSID: ownerSID, StateRoot: layout.UpdateStateRoot}
	journal, err := loadWindowsActivationJournal(updateConfig)
	if errors.Is(err, os.ErrNotExist) {
		if err := config.Source.Verify(layout.Binary); err == nil {
			return &hostinstall.WindowsRollbackIdentity{Source: config.Source}, noop, nil
		}
		// Native repair can select the previously authenticated signed slot.
		// Its durable rollback identity remains the proof after journal retirement.
		if !config.RollbackSigned || config.RollbackSource == nil {
			return nil, nil, errInvalidWindowsActivation
		}
		identity := *config.RollbackSource
		if err := identity.Verify(layout.Binary); err != nil {
			return nil, nil, err
		}
		source := workerupdate.TUFSource{RepositoryURL: config.Artifact.RepositoryURL, StateRoot: filepath.Join(layout.UpdateStateRoot, "tuf")}
		if err := source.AuthorizeRecovery(ctx, identity.Version, "windows", identity.Architecture); err != nil {
			return nil, nil, err
		}
		return &hostinstall.WindowsRollbackIdentity{Source: identity, Signed: true}, noop, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !windowsMachineFileSecurityMatches(windowsActivationJournalPath(layout.UpdateStateRoot), "D:P(A;;FA;;;SY)(A;;FA;;;BA)") {
		return nil, nil, errInvalidWindowsActivation
	}
	if !nativeWindowsJournalRetirable(journal) {
		return nil, nil, fmt.Errorf("finish or recover the pending update before reinstalling: %w", ErrWindowsActivationUnavailable)
	}
	sourceIdentity, signed, err := nativeWindowsRollbackSource(journal)
	if err != nil {
		return nil, nil, err
	}
	sourceIdentity.AutomaticUpdates = config.Source.AutomaticUpdates
	if err := sourceIdentity.Verify(layout.Binary); err != nil {
		return nil, nil, err
	}
	target := workerupdate.ComponentTarget{SHA256: sourceIdentity.SHA256, Length: sourceIdentity.Length, Platform: "windows", Architecture: sourceIdentity.Architecture}
	if err := verifyWindowsStableBinary(ctx, layout.Binary, target, ownerSID); err != nil {
		return nil, nil, err
	}
	if signed {
		source := workerupdate.TUFSource{RepositoryURL: config.Artifact.RepositoryURL, StateRoot: filepath.Join(layout.UpdateStateRoot, "tuf")}
		if err := source.AuthorizeRecovery(ctx, sourceIdentity.Version, "windows", sourceIdentity.Architecture); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := os.Remove(windowsActivationJournalPath(layout.UpdateStateRoot)); err != nil {
		return nil, nil, err
	}
	return &hostinstall.WindowsRollbackIdentity{Source: sourceIdentity, Signed: signed}, func() error { return (&windowsSCMActivationBackend{config: updateConfig}).WriteJournal(journal) }, nil
}
