//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"io"
	"os"
	"path/filepath"
	"time"
)

type guardFileBackup struct {
	path, backup      string
	original, current os.FileInfo
}
type guardFileRollback struct{ files []*guardFileBackup }

func snapshotGuardFiles(paths ...string) (*guardFileRollback, error) {
	return snapshotGuardFilesValidated(validateOwnedRootStateFile, paths...)
}

func snapshotGuardFilesValidated(validate func(string, string, os.FileInfo) error, paths ...string) (*guardFileRollback, error) {
	defer diagnostics.UpdateStep("guard_rollback_snapshot")()
	rollback := &guardFileRollback{}
	for _, path := range paths {
		item := &guardFileBackup{path: path}
		rollback.files = append(rollback.files, item)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			rollback.cleanup()
			return nil, err
		}
		if !info.Mode().IsRegular() {
			rollback.cleanup()
			return nil, errors.New("unsafe existing guard installation; preserved")
		}
		if err := validate(path, filepath.Base(path), info); err != nil {
			rollback.cleanup()
			return nil, err
		}
		source, err := os.Open(path)
		if err != nil {
			rollback.cleanup()
			return nil, err
		}
		opened, err := source.Stat()
		if err != nil || !os.SameFile(info, opened) {
			source.Close()
			rollback.cleanup()
			return nil, errors.New("guard installation changed while taking rollback copy")
		}
		file, err := os.CreateTemp(filepath.Dir(path), ".paperboat-guard-prior-")
		if err != nil {
			source.Close()
			rollback.cleanup()
			return nil, err
		}
		item.backup = file.Name()
		item.original = info
		item.current = info
		copied, err := atomicfile.SyncedCopy(file, io.LimitReader(source, info.Size()))
		if err == nil && copied != info.Size() {
			err = io.ErrUnexpectedEOF
		}
		err = errors.Join(err, source.Close())
		if err == nil {
			err = file.Chmod(info.Mode().Perm())
		}
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
		if err != nil {
			rollback.cleanup()
			return nil, err
		}
	}
	return rollback, nil
}
func (r *guardFileRollback) record(path string) {
	for _, item := range r.files {
		if item.path == path {
			item.current, _ = os.Lstat(path)
			if item.current != nil {
				// On Windows os.SameFile reads a file's identity (volume and index) when first asked, not when the
				// info was taken, so a replaced file would compare equal to itself. Asking now pins this one.
				os.SameFile(item.current, item.current)
			}
			return
		}
	}
}
func (r *guardFileRollback) restore() error {
	for _, item := range r.files {
		current, err := os.Lstat(item.path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (item.current == nil || !os.SameFile(current, item.current) || !current.Mode().IsRegular()) {
			return fmt.Errorf("guard installation changed externally; rollback copy retained at %s", item.backup)
		}
		if item.original != nil {
			if current != nil && os.SameFile(current, item.original) {
				continue
			}
			if err := replaceGuardFile(item.backup, item.path); err != nil {
				return fmt.Errorf("restore prior guard installation from %s: %w", item.backup, err)
			}
			item.backup = ""
		} else if current != nil {
			if err := removeGuardImage(item.path, 10*time.Second); err != nil {
				return err
			}
		}
	}
	return nil
}
func (r *guardFileRollback) cleanup() {
	for _, item := range r.files {
		if item.backup != "" {
			_ = os.Remove(item.backup)
		}
	}
}
func finishGuardInstallation(ctx context.Context, r *guardFileRollback, result *error, stop, restore func(context.Context) error) {
	if *result == nil {
		r.cleanup()
		return
	}
	*result = errors.Join(*result, restoreGuardInstallation(ctx, r, stop, restore))
}

// restoreGuardInstallation stops the installed guard, puts the prior files back and starts the prior guard.
func restoreGuardInstallation(ctx context.Context, r *guardFileRollback, stop, restore func(context.Context) error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if stop != nil {
		if err := stop(cleanup); err != nil {
			return fmt.Errorf("stop failed guard candidate before restoring installation: %w", err)
		}
	}
	err := r.restore()
	if err == nil {
		r.cleanup()
	}
	// When the files changed under us (another installer put a guard in place) the copy is kept, but the
	// guard is never left stopped: start whatever is installed (ubuntu24, 2026-10-26: a failed update's
	// undo stopped the guard the updater had just healed and left the host without one).
	if restore != nil {
		err = errors.Join(err, restore(cleanup))
	}
	return err
}

// Undo restores the guard installation that an Install replaced: its executable and service files, then
// its state and service. An update keeps it until the whole update commits, so a failed update puts back
// every component, the guard included (a failed update used to leave the new guard behind).
type Undo struct {
	rollback      *guardFileRollback
	stop, restore func(context.Context) error
}

// Restore stops the installed guard and restores the prior one. A nil Undo restores nothing.
func (u *Undo) Restore(ctx context.Context) error {
	if u == nil {
		return nil
	}
	return restoreGuardInstallation(ctx, u.rollback, u.stop, u.restore)
}

// Discard keeps the installed guard and drops the copies of the prior one.
func (u *Undo) Discard() {
	if u != nil {
		u.rollback.cleanup()
	}
}

// Install installs and starts the guard from executable. On success (trust may still be pending) it
// returns the Undo that restores the prior installation; the caller restores or discards it.
func Install(ctx context.Context, executable, browserDomain string) (*Undo, error) {
	var undo *Undo
	err := installGuard(ctx, executable, browserDomain, &undo)
	return undo, err
}
