//go:build windows

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

var errWindowsUserStateRefused = errors.New("a Paperboat user state path is outside the owner's profile or passes through a link; it was not removed")

// checkWindowsUserStatePath refuses a user state path that is not below the owner's profile, or that is or lies
// below a reparse point. It keeps an honest mistake away from other places; it cannot stop a link planted after
// the check, which is why the removal runs as the user and administrator rights are used only by handle
// (retryWindowsRemoval). Below the path nothing is followed: os.RemoveAll removes a link as an entry, and
// removePathPreserving refuses to descend into one.
func checkWindowsUserStatePath(profile, path string) error {
	current := filepath.Clean(profile)
	relative, err := filepath.Rel(current, filepath.Clean(path))
	if err != nil || relative == "." || !filepath.IsLocal(relative) {
		return errWindowsUserStateRefused
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if unsafeTraversal, err := unsafeCleanupPathTraversal(current, info); err != nil {
			return err
		} else if unsafeTraversal {
			return errWindowsUserStateRefused
		}
	}
	return nil
}

// afterWindowsUserStateCheck runs between the path check and the removal; tests plant a link there.
var afterWindowsUserStateCheck = func() {}

// removeWindowsStateElevated is removeWindowsStateByHandle; tests replace it to prove that a removal needed
// only the user's rights.
var removeWindowsStateElevated = removeWindowsStateByHandle

// retryWindowsRemoval removes one user state path for the elevated helper. The user can write there, so a link
// can appear at any moment, also between the check and the removal. The removal therefore runs as the user
// without the helper's elevation (what MSI and Chromium's installer do for per-user data): whatever a planted
// link points at, only what the user could delete anyway can be deleted. Only what the user's rights were
// refused is then removed with the helper's elevation, by handle (removeWindowsStateByHandle).
func retryWindowsRemoval(ctx context.Context, profile, path string, preserved []string) error {
	for {
		checked := false
		last := windowssecurity.WithUnelevatedToken(func() error {
			// Before every attempt: the user can change these directories while the helper waits.
			if err := checkWindowsUserStatePath(profile, path); err != nil {
				return err
			}
			checked = true
			afterWindowsUserStateCheck()
			return removePathPreserving(path, preserved)
		})
		if checked && errors.Is(last, fs.ErrPermission) {
			last = removeWindowsStateElevated(path, preserved)
		}
		if last == nil || errors.Is(last, errWindowsUserStateRefused) {
			return last
		}
		select {
		case <-ctx.Done():
			return windowsUninstallLocalFailure("", last, ctx.Err(), context.Cause(ctx))
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// removeWindowsStateByHandle removes, with the helper's elevation, what is left of a user state path after the
// user's own rights were refused: entries that only Administrators or LocalSystem may delete, left there by the
// account's services or by a pb that ran elevated. Ownership does not decide it: what an elevated pb creates
// usually still grants the user, and is gone before this runs. The user can swap anything in these directories
// for a link at any moment, so nothing here goes by path: the parent directory is opened once, Windows confirms
// that this handle is the directory the path names (so no link led to it), and everything below is removed
// relative to that handle without following a link (removeBelowDirectory).
func removeWindowsStateByHandle(path string, preserved []string) (result error) {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, parent.Close()) }()
	if err := checkWindowsStateDirectory(parent, filepath.Dir(path)); err != nil {
		return err
	}
	return removeBelowDirectory(parent, filepath.Base(path), path, preserved, checkWindowsStateDirectory)
}

// checkWindowsStateDirectory refuses an open directory that is not the one path names: Windows reports where
// the handle really is, and a link anywhere on the way gives another place.
func checkWindowsStateDirectory(directory *os.Root, path string) (result error) {
	self, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, self.Close()) }()
	if elevation.VerifyHandleFinalPath(windows.Handle(self.Fd()), path) != nil {
		return errWindowsUserStateRefused
	}
	return nil
}
