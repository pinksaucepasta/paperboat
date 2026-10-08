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
		*result = errors.Join(*result, err)
		return
	}
	r.cleanup()
	if restore != nil {
		*result = errors.Join(*result, restore(cleanup))
	}
}
