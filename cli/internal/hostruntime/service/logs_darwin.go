//go:build darwin

package service

import (
	"errors"
	"os/user"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

func prepareNativeServiceLogs(config Config, definition []byte) error {
	var paths struct {
		Out string `plist:"StandardOutPath"`
		Err string `plist:"StandardErrorPath"`
	}
	if _, err := plist.Unmarshal(definition, &paths); err != nil {
		return err
	}
	if paths.Out == "" && paths.Err == "" {
		return nil
	}
	account, err := user.Lookup(config.User)
	if err != nil {
		return err
	}
	group, err := user.LookupGroup(config.Group)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	for _, path := range []string{paths.Out, paths.Err} {
		if path == "" {
			continue
		}
		if err := prepareServiceLog(path, uid, gid); err != nil {
			return err
		}
		if paths.Out == paths.Err {
			break
		}
	}
	return nil
}

// The trusted parent owns the pathname; the service owns only its log file.
// Never follow links or truncate existing diagnostics while repairing ownership.
func prepareServiceLog(path string, uid, gid int) error {
	parent, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	var directory unix.Stat_t
	if err := unix.Fstat(parent, &directory); err != nil {
		return err
	}
	if directory.Uid != 0 || directory.Mode&0022 != 0 {
		return ErrInvalidDefinition
	}
	fd, err := unix.Openat(parent, filepath.Base(path), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(parent, filepath.Base(path), unix.O_WRONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Nlink != 1 || (info.Uid != 0 && info.Uid != uint32(uid)) {
		return ErrInvalidDefinition
	}
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return err
	}
	if err := unix.Fchmod(fd, 0600); err != nil {
		return err
	}
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Uid != uint32(uid) || info.Gid != uint32(gid) || info.Mode&0777 != 0600 {
		return ErrInvalidDefinition
	}
	return nil
}
