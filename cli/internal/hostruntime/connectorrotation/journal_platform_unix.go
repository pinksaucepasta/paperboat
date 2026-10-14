//go:build darwin || linux

package connectorrotation

import (
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"golang.org/x/sys/unix"
)

var errJournalFileNotExist = os.ErrNotExist

func journalRecoveryEligible(err error) bool {
	return errorLeavesMatch(err, ErrJournalCorrupt)
}

func ensurePrivateJournalDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return journalOperationFailure(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return journalOperationFailure(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return ErrInvalidConfig
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrInvalidConfig
	}
	return nil
}

func readPrivateJournalFile(path string, limit int64) (body []byte, readErr error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errorLeavesMatch(err, os.ErrNotExist) {
		return nil, errJournalFileNotExist
	}
	if errorLeavesMatch(err, unix.ELOOP) {
		return nil, safeRotationFailure("connector rotation journal path is not a private regular file", errors.Join(ErrJournalCorrupt, errJournalSecurity, err))
	}
	if err != nil {
		return nil, journalOperationFailure(err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		closeErr := unix.Close(fd)
		return nil, safeRotationFailure("connector rotation journal file could not be opened safely", errors.Join(ErrJournalUncertain, closeErr))
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			body = nil
			if readErr == nil {
				readErr = journalOperationFailure(closeErr)
			} else {
				readErr = safeRotationFailure("connector rotation journal file could not be closed", errors.Join(readErr, closeErr))
			}
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, journalOperationFailure(err)
	}
	pathInfo, pathErr := os.Lstat(path)
	if pathErr != nil {
		return nil, journalOperationFailure(pathErr)
	}
	if !opened.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, pathInfo) || opened.Mode().Perm() != 0o600 || opened.Size() < 0 || opened.Size() > limit {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	body, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, journalOperationFailure(err)
	}
	if int64(len(body)) != opened.Size() || int64(len(body)) > limit {
		return nil, ErrJournalCorrupt
	}
	return body, nil
}

func writePrivateJournalFile(path string, body []byte) error {
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
