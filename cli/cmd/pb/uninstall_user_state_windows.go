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

var errWindowsUserStateRefused = errors.New("a Paperboat user state path is outside the owner's profile, passes through a link or belongs to another account; it was not removed")

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
// refused is then removed with the helper's elevation, by handle (removeWindowsStateByHandle). An empty request
// directory that another process holds open is not retried for five minutes: it is returned in leftovers and
// reported in the status (lockedBridgeLeftover).
func retryWindowsRemoval(ctx context.Context, profile, path string, preserved []string, leftovers *[]string) error {
	for attempt := 1; ; attempt++ {
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
		if leftover, ok := lockedBridgeLeftover(last); ok && leftovers != nil {
			*leftovers = appendLeftover(*leftovers, leftover)
			return nil
		}
		// Only a file another process still holds is worth waiting for; any other failure is reported as itself.
		if !retryableWindowsRemoval(last) && attempt >= maxWindowsRemovalAttempts {
			return windowsUninstallLocalFailure("", last)
		}
		select {
		case <-ctx.Done():
			return windowsUninstallLocalFailure("", last, ctx.Err(), context.Cause(ctx))
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// maxWindowsRemovalAttempts bounds the attempts at a removal that fails for a reason other than a held file.
const maxWindowsRemovalAttempts = 3

// retryableWindowsRemoval reports a failure that clears once another process (a stopping service, a scanner)
// lets go of a file: a sharing or lock violation, a directory still listing an entry pending deletion, or the
// access denial a delete-pending file returns.
func retryableWindowsRemoval(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) || errors.Is(err, fs.ErrPermission)
}

// removeWindowsStateByHandle removes, with the helper's elevation, what is left of a user state path after the
// user's own rights were refused: entries that only Administrators or LocalSystem may delete, left there by the
// account's services or by a pb that ran elevated. Ownership does not decide whether it is needed: what an
// elevated pb creates usually still grants the user, and is gone before this runs. The user can swap anything
// in these directories for a link at any moment, so nothing here goes by path and no earlier check is trusted:
// every directory, from the parent down, is opened once and checked on that handle
// (checkWindowsStateDirectory) before anything in it is touched, and its entries are removed relative to that
// handle without following a link (removeBelowDirectory).
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

// checkWindowsStateDirectory refuses an open directory unless the handle itself shows that it is the one path
// names (Windows reports where the handle really is; a link anywhere on the way gives another place), that it
// is not a link, and that it belongs to the user, Administrators or LocalSystem: the accounts that create
// Paperboat state. The plan's owner check was made by path, earlier; this one is on what is about to be emptied.
func checkWindowsStateDirectory(directory *os.Root, path string) (result error) {
	self, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, self.Close()) }()
	handle := windows.Handle(self.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || elevation.VerifyHandleFinalPath(handle, path) != nil {
		return errWindowsUserStateRefused
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return err
	}
	owners := []*windows.SID{user.User.Sid}
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinLocalSystemSid} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return err
		}
		owners = append(owners, sid)
	}
	for _, owner := range owners {
		if matches, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, owner); err != nil {
			return err
		} else if matches {
			return nil
		}
	}
	return errWindowsUserStateRefused
}

// appendLeftover records a leftover path once: every retry of the same removal reports the same directory.
func appendLeftover(leftovers []string, leftover string) []string {
	for _, existing := range leftovers {
		if strings.EqualFold(existing, leftover) {
			return leftovers
		}
	}
	return append(leftovers, leftover)
}
