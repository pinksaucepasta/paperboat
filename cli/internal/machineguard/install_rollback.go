//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"errors"
	"fmt"
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
		_, err = io.CopyN(file, source, info.Size())
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
			if err := replaceStateFile(item.backup, item.path); err != nil {
				return fmt.Errorf("restore prior guard installation from %s: %w", item.backup, err)
			}
			item.backup = ""
		} else if current != nil {
			if err := os.Remove(item.path); err != nil {
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
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if stop != nil {
		if err := stop(cleanup); err != nil {
			*result = errors.Join(*result, fmt.Errorf("stop failed guard candidate before restoring installation: %w", err))
			return
		}
	}
	if err := r.restore(); err != nil {
		// The files changed under us (another installer put a guard in place); keep the copy, but never
		// leave the guard stopped: start whatever is installed (ubuntu24, 2026-10-26: a failed update's
		// undo stopped the guard the updater had just healed and left the host without one).
		*result = errors.Join(*result, err)
		if restore != nil {
			*result = errors.Join(*result, restore(cleanup))
		}
		return
	}
	r.cleanup()
	if restore != nil {
		*result = errors.Join(*result, restore(cleanup))
	}
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
	err := errRestoreRequested
	finishGuardInstallation(ctx, u.rollback, &err, u.stop, u.restore)
	if errors.Is(err, errRestoreRequested) && len(unwrapJoined(err)) == 1 {
		return nil
	}
	return errors.Join(unwrapJoinedExcept(err, errRestoreRequested)...)
}

// Discard keeps the installed guard and drops the copies of the prior one.
func (u *Undo) Discard() {
	if u != nil {
		u.rollback.cleanup()
	}
}

var errRestoreRequested = errors.New("guard restore requested")

func unwrapJoined(err error) []error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

func unwrapJoinedExcept(err, skip error) []error {
	var kept []error
	for _, item := range unwrapJoined(err) {
		if item != skip {
			kept = append(kept, item)
		}
	}
	return kept
}

// Install installs and starts the guard from executable. On success (trust may still be pending) it
// returns the Undo that restores the prior installation; the caller restores or discards it.
func Install(ctx context.Context, executable, browserDomain string) (*Undo, error) {
	var undo *Undo
	err := installGuard(ctx, executable, browserDomain, &undo)
	return undo, err
}
