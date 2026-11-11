//go:build !windows

// Package ownerpath checks that no other account can replace a private directory through one of its ancestors.
package ownerpath

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// CheckAncestors reports an error unless every ancestor of dir is a directory owned by uid or root that no other
// account can write to (a root-owned sticky directory such as /tmp, or group write by the owner's private group, is
// fine). A symlink owned by uid or root (macOS
// /var and /tmp) is accepted when its resolved path passes the same check. dir itself is the caller's to check.
// Prior art: daemonrpc's cleanup walk and machineguard protectedDirectoryPath.
func CheckAncestors(dir string, uid int) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errors.New("private directory path must be absolute and clean")
	}
	return checkChain(filepath.Dir(dir), uid, true)
}

func checkChain(path string, uid int, followLinks bool) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: owner unavailable", current)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(current)
			if !followLinks || (int(stat.Uid) != uid && stat.Uid != 0) || err != nil {
				return errors.Join(fmt.Errorf("%s is a link another account controls", current), err)
			}
			if err := checkChain(resolved, uid, false); err != nil {
				return err
			}
		} else if !trusted(info, stat, uid) {
			return fmt.Errorf("%s can be changed by another account (owner %d, mode %s)", current, stat.Uid, info.Mode())
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func trusted(info os.FileInfo, stat *syscall.Stat_t, uid int) bool {
	if !info.IsDir() || (int(stat.Uid) != uid && stat.Uid != 0) {
		return false
	}
	if stat.Uid == 0 && info.Mode()&os.ModeSticky != 0 {
		return true
	}
	perm := info.Mode().Perm()
	return perm&0o002 == 0 && (perm&0o020 == 0 || privateGroup(stat))
}

// privateGroup reports a directory whose group is its owner's own user-private group (named after the owner,
// as useradd's USERGROUPS_ENAB makes it), so group write under umask 002 grants no one else. Prior art: Debian's
// OpenSSH user-group-modes patch accepts the same. shortcut: the group's member list is not read; a user who adds
// others to their private group gives them this access anyway.
func privateGroup(stat *syscall.Stat_t) bool {
	owner, err := user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10))
	if err != nil {
		return false
	}
	group, err := user.LookupGroupId(strconv.FormatUint(uint64(stat.Gid), 10))
	return err == nil && stat.Uid != 0 && group.Name == owner.Username
}
