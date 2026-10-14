//go:build !windows

package daemonrpc

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A process lock fences both stale-socket cleanup and the entire listener
// lifetime. The protected parent prevents another OS user replacing either.
func Listen(addr string) (net.Listener, error) {
	path := strings.TrimPrefix(addr, "unix://")
	if !filepath.IsAbs(path) {
		return nil, errors.New("daemon socket path must be absolute")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0022 != 0 || int(st.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("daemon socket directory must be owned by user %d and not writable by others (mode %s, stat type %T)", os.Geteuid(), info.Mode(), info.Sys())
	}
	lockPath := path + ".lock"
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another daemon owns this socket")
	}
	success := false
	defer func() {
		if !success {
			syscall.Flock(fd, syscall.LOCK_UN)
			lock.Close()
		}
	}()
	if info, err = os.Lstat(path); err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || int(st.Uid) != os.Geteuid() {
			return nil, errors.New("refusing to remove an unowned or non-socket daemon path")
		}
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, errors.New("daemon socket is already active")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot establish stale daemon socket: %w", dialErr)
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	success = true
	return &ownedListener{Listener: listener, lock: lock}, nil
}

type ownedListener struct {
	net.Listener
	lock *os.File
	once sync.Once
	err  error
}

func (l *ownedListener) Close() error {
	l.once.Do(func() {
		l.err = l.Listener.Close()
		syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN)
		l.err = errors.Join(l.err, l.lock.Close())
	})
	return l.err
}
