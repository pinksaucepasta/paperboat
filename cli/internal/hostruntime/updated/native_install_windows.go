//go:build windows

package updated

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// PrepareWindowsNativeInstall is called with the instance installation mutex
// held, after synchronously stopping the updater. It retires only completed
// transactions; the returned callback restores their policy state on failure,
// before the prior services restart. Signed metadata and release slots remain.
func PrepareWindowsNativeInstall(ctx context.Context, ownerSID string) (func() error, error) {
	noop := func() error { return nil }
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return nil, err
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if errors.Is(err, os.ErrNotExist) {
		return noop, nil
	}
	if err != nil {
		return nil, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer manager.Disconnect()
	updater, err := manager.OpenService(windowsUpdaterService + "-" + layout.Instance)
	if err == nil {
		status, queryErr := updater.Query()
		updater.Close()
		if queryErr != nil {
			return nil, queryErr
		}
		if status.State != svc.Stopped {
			return nil, fmt.Errorf("stop the updater before reinstalling: %w", ErrWindowsActivationUnavailable)
		}
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, err
	}
	activator, err := manager.OpenService(windowsActivatorService + "-" + layout.Instance)
	if err == nil {
		activator.Close()
		return nil, fmt.Errorf("finish or recover the pending update before reinstalling: %w", ErrWindowsActivationUnavailable)
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, err
	}
	updateConfig := WindowsConfig{OwnerSID: ownerSID, StateRoot: layout.UpdateStateRoot, SetupMode: config.SetupMode}
	journal, err := loadWindowsActivationJournal(updateConfig)
	if errors.Is(err, os.ErrNotExist) {
		return noop, nil
	}
	if err != nil {
		return nil, err
	}
	if !nativeWindowsJournalRetirable(journal) {
		return nil, fmt.Errorf("finish or recover the pending update before reinstalling: %w", ErrWindowsActivationUnavailable)
	}
	if err := os.Remove(windowsActivationJournalPath(layout.UpdateStateRoot)); err != nil {
		return nil, err
	}
	return func() error { return (&windowsSCMActivationBackend{config: updateConfig}).WriteJournal(journal) }, nil
}
