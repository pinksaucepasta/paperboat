//go:build !windows

package daemonrpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// cleanupCanonicalAddress removes the persistent RPC lock only after its caller
// has confirmed daemon termination. Ordinary listener shutdown keeps the lock.
func cleanupCanonicalAddress(ctx context.Context, address string) error {
	path := strings.TrimPrefix(address, "unix://")
	if filepath.Base(path) != "daemon.sock" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("invalid daemon cleanup address")
	}
	// Accept only finite platform runtime namespaces, never an arbitrary
	// PAPERBOAT_DAEMON_SOCK override supplied to the process.
	allowed, err := cleanupAddresses()
	if err != nil {
		return err
	}
	matched := false
	for _, candidate := range allowed {
		if path == candidate {
			matched = true
		}
	}
	if !matched {
		return errors.New("invalid daemon cleanup namespace")
	}
	return cleanupUnusedAddress(ctx, path)
}

// CleanupUnusedAddresses includes Linux's finite default fallback locations
// because the socket's removal can already have changed DefaultAddress.
func CleanupUnusedAddresses(ctx context.Context) error {
	addresses, err := cleanupAddresses()
	if err != nil {
		return err
	}
	var result error
	seen := map[string]bool{}
	for _, path := range addresses {
		if seen[path] {
			continue
		}
		seen[path] = true
		if ctx == nil {
			return errors.New("missing daemon cleanup context")
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		result = errors.Join(result, cleanupCanonicalAddress(ctx, path))
	}
	return result
}

func cleanupAddresses() ([]string, error) {
	if runtime.GOOS == "darwin" {
		paths, err := localapi.CurrentPaths(os.Geteuid())
		if err != nil {
			return nil, err
		}
		return []string{filepath.Join(paths.RuntimeRoot, "daemon.sock")}, nil
	}
	allowed := []string{filepath.Join("/run/user", fmt.Sprint(os.Geteuid()), "paperboat", "daemon.sock"), filepath.Join(os.TempDir(), fmt.Sprintf("paperboat-%d", os.Geteuid()), "daemon.sock")}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		allowed = append(allowed, filepath.Join(xdg, "paperboat", "daemon.sock"))
	}
	return allowed, nil
}

func cleanupUnusedAddress(ctx context.Context, path string) (resultErr error) {
	if ctx == nil {
		return errors.New("missing daemon cleanup context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (int(st.Uid) != os.Geteuid() && st.Uid != 0) || info.Mode().Perm()&0022 != 0 && !(st.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
			return errors.New("untrusted daemon cleanup ancestor")
		}
		if current == dir && (int(st.Uid) != os.Geteuid() || info.Mode().Perm() != 0700) {
			return errors.New("untrusted daemon cleanup directory")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("daemon socket still present")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lockPath := path + ".lock"
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return removeEmptyRuntimeDirectory(dir)
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), lockPath)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return errors.New("untrusted daemon cleanup lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, syscall.Flock(fd, syscall.LOCK_UN)) }()
	named, err := os.Lstat(lockPath)
	if err != nil {
		return err
	}
	if !os.SameFile(info, named) {
		return errors.New("daemon cleanup lock changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(lockPath); err != nil {
		return err
	}
	return removeEmptyRuntimeDirectory(dir)
}

func removeEmptyRuntimeDirectory(dir string) error {
	err := os.Remove(dir)
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
