//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type processLock struct {
	file *os.File
}

func acquireProcessLock(path string, uid int) (*processLock, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || uid < 0 {
		return nil, ErrInvalidInventoryConfig
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	closeWith := func(cause error) (*processLock, error) {
		return nil, errors.Join(cause, file.Close())
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || fileOwner(info) != uid {
		if err != nil {
			return closeWith(err)
		}
		return closeWith(localapi.ErrUnsafeSocket)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		if err != nil {
			return closeWith(err)
		}
		return closeWith(localapi.ErrUnsafeSocket)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeWith(localapi.ErrAlreadyRunning)
		}
		return closeWith(err)
	}
	err = file.Truncate(0)
	if err == nil {
		_, err = file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	if err != nil {
		unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeWith(errors.Join(fmt.Errorf("record local daemon PID: %w", err), unlockErr))
	}
	return &processLock{file: file}, nil
}

// lockHandover bounds how long a new daemon generation waits for the one before it to release the
// daemon lock: pb install starts the new unit, then sends the old one SIGQUIT.
const lockHandover = time.Minute

// DaemonLock is the held daemon lock: it also decides whether a stale local API socket may go.
type DaemonLock interface {
	io.Closer
	localapi.StaleSocketAuthority
}

// WaitDaemonLock takes the daemon lock, waiting up to lockHandover while another generation holds it.
func WaitDaemonLock(ctx context.Context, path string, uid int) (DaemonLock, error) {
	ctx, cancel := context.WithTimeout(ctx, lockHandover)
	defer cancel()
	for {
		lock, err := acquireProcessLock(path, uid)
		if !errors.Is(err, localapi.ErrAlreadyRunning) {
			if err != nil {
				return nil, err
			}
			return lock, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(err, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (l *processLock) CanRemoveStaleSocket(_ context.Context, _ string) bool {
	return l != nil && l.file != nil
}

func (l *processLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return errors.Join(unlockErr, file.Close())
}

func fileOwner(info os.FileInfo) int {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid)
	}
	return -1
}
