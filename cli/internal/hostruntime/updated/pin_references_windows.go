//go:build windows

package updated

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// ProtectedWindowsPinReferences projects only the updater's authoritative
// protected transaction. Callers invoke it under native publication custody.
func ProtectedWindowsPinReferences(ctx context.Context, instance string) ([]string, error) {
	if ctx == nil {
		return nil, errInvalidWindowsActivation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	installed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return nil, err
	}
	layout, err := service.WindowsUserLayout(installed.OwnerSID)
	if err != nil || layout.Instance != instance {
		return nil, errInvalidWindowsActivation
	}
	config := WindowsConfig{OwnerSID: installed.OwnerSID, StateRoot: layout.UpdateStateRoot}
	// Absence is meaningful only inside the updater's protected real directory.
	// A replaced parent must never make a pending transaction appear absent.
	for path := filepath.Clean(config.StateRoot); ; path = filepath.Dir(path) {
		attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
		if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return nil, errors.Join(errInvalidWindowsActivation, err)
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil || !windowssecurity.OwnerMatchesSID(config.StateRoot, system) || !windowssecurity.ProtectedDACLMatches(config.StateRoot, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)") {
		return nil, errors.Join(errInvalidWindowsActivation, err)
	}
	journal, err := loadWindowsActivationJournal(config)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	refs := []string{journal.Runtime.Path, journal.CLI.Path, journal.Hostd.Path, journal.Updater.Path, journal.PreviousBinary.Path}
	if journal.PreviousRuntime.Path != "" {
		refs = append(refs, journal.PreviousRuntime.Path)
	}
	for _, target := range []windowsServiceTarget{journal.OldHostd, journal.NewHostd, journal.OldUpdater, journal.NewUpdater, journal.OldSSH, journal.NewSSH} {
		if target.Executable != "" {
			refs = append(refs, target.Executable)
		}
	}
	if journal.PreviousSource != nil {
		path, err := hostinstall.WindowsPinnedSourcePath(layout, *journal.PreviousSource)
		if err != nil {
			return nil, err
		}
		refs = append(refs, path)
	}
	return refs, nil
}
