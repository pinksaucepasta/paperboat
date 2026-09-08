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

func ensurePrivateJournalDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return ErrInvalidConfig
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrInvalidConfig
	}
	return nil
}

func readPrivateJournalFile(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, errJournalFileNotExist
	}
	if errors.Is(err, unix.ELOOP) {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrJournalCorrupt
	}
	defer file.Close()
	opened, err := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !opened.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, pathInfo) || opened.Mode().Perm() != 0o600 || opened.Size() < 0 || opened.Size() > limit {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) != opened.Size() || int64(len(body)) > limit {
		return nil, ErrJournalCorrupt
	}
	return body, nil
}

func writePrivateJournalFile(path string, body []byte) error {
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
