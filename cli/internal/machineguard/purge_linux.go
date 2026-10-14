//go:build linux

package machineguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Purge completely removes the machine guard, including the deny-only
// protection that Uninstall deliberately retains. It is used only when no
// Paperboat installation remains on the machine.
func Purge(ctx context.Context) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	return purgeLinux(ctx, linuxPurge{
		Paths: linuxPurgePaths{
			UnitDir:   "/etc/systemd/system",
			StateDir:  DefaultStateDir,
			Libexec:   []string{"/usr/local/libexec/paperboat-machineguard", "/usr/local/libexec/paperboat-deviceguard"},
			TrustDirs: []string{"/usr/local/share/ca-certificates", "/etc/ssl/certs"},
		},
		Run: func(ctx context.Context, name string, args ...string) error {
			output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %v: %w: %s", name, args, err, output)
			}
			return nil
		},
		Uninstall: func(ctx context.Context) error {
			_, err := Uninstall(ctx)
			return err
		},
		LocalNames: func(ctx context.Context) error {
			return cleanupHistoricalLocalNames(ctx, Config{StateDir: DefaultStateDir})
		},
	})
}

type linuxPurgePaths struct {
	UnitDir   string
	StateDir  string
	Libexec   []string
	TrustDirs []string
}

// linuxPurge carries every external effect so the purge order can be tested
// against temporary roots and fake commands.
type linuxPurge struct {
	Paths      linuxPurgePaths
	Run        func(context.Context, string, ...string) error
	Uninstall  func(context.Context) error
	LocalNames func(context.Context) error
}

func purgeLinux(ctx context.Context, p linuxPurge) error {
	paths := p.Paths
	var result error
	// The guard's own uninstall removes listeners, names and owned trust while
	// its state is intact; a missing or damaged guard must not block removal.
	if _, err := os.Stat(paths.StateDir); err == nil {
		if err := p.Uninstall(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("guard uninstall: %w", err))
		}
	} else if !os.IsNotExist(err) {
		result = errors.Join(result, err)
	}
	for _, unit := range []string{"paperboat-machineguard.service", "paperboat-machineguard-deny.service"} {
		// A unit that is not installed has nothing to stop; an installed one must stop.
		_, statErr := os.Lstat(filepath.Join(paths.UnitDir, unit))
		if err := p.Run(ctx, "systemctl", "disable", "--now", unit); err != nil && statErr == nil {
			result = errors.Join(result, fmt.Errorf("stop %s: %w", unit, err))
		}
	}
	unitFiles, _ := filepath.Glob(filepath.Join(paths.UnitDir, "paperboat-machineguard*"))
	for _, path := range unitFiles {
		result = errors.Join(result, removeOwnedGuardUnit(path))
	}
	links, _ := filepath.Glob(filepath.Join(paths.UnitDir, "*", "paperboat-machineguard*"))
	for _, path := range links {
		result = errors.Join(result, removeOwnedPath(path))
	}
	result = errors.Join(result, p.Run(ctx, "systemctl", "daemon-reload"))
	// The deny table outlives the guard by design; a complete purge removes it.
	if p.Run(ctx, "nft", "list", "table", "inet", guardTable) == nil {
		result = errors.Join(result, p.Run(ctx, "nft", "delete", "table", "inet", guardTable))
	}
	trustChanged := false
	for _, directory := range paths.TrustDirs {
		matches, _ := filepath.Glob(filepath.Join(directory, "paperboat-machineguard-*"))
		for _, path := range matches {
			trustChanged = true
			result = errors.Join(result, removeOwnedPath(path))
		}
	}
	if trustChanged {
		result = errors.Join(result, p.Run(ctx, "update-ca-certificates", "--fresh"))
	}
	// Local names are not recorded in state, so this runs even when the guard
	// uninstall above was skipped or failed; a retry must still reach them.
	result = errors.Join(result, p.LocalNames(ctx))
	for _, path := range paths.Libexec {
		result = errors.Join(result, removeOwnedPath(path))
	}
	// State is the resumable record. Remove it only when every earlier step
	// succeeded, so a retry still sees what it must clean up.
	if result == nil {
		result = errors.Join(result, removeOwnedPath(paths.StateDir))
	} else {
		result = errors.Join(result, fmt.Errorf("machine guard state %s retained; fix the errors above and retry", paths.StateDir))
	}
	return result
}

// removeOwnedGuardUnit removes a unit only when it carries the guard's ownership
// marker, the same rule install and uninstall apply before rewriting it.
func removeOwnedGuardUnit(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsafe machine guard unit %s; preserved", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte(guardUnitMarker)) {
		return fmt.Errorf("refusing to remove foreign machine guard unit %s", path)
	}
	return removeOwnedPath(path)
}

func removeOwnedPath(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
