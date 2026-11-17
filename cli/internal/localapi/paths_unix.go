//go:build darwin || linux

package localapi

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/pinksaucepasta/paperboat/cli/internal/ownerpath"
)

type Paths struct {
	StateRoot   string
	RuntimeRoot string
	SocketPath  string
	LockPath    string
}

func CurrentPaths(uid int) (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	environ := func(name string) string {
		return os.Getenv(name)
	}
	return ResolvePaths(environ, home, uid)
}

func ResolvePaths(environ func(string) string, home string, uid int) (Paths, error) {
	if environ == nil || !filepath.IsAbs(home) || uid < 0 {
		return Paths{}, ErrInvalidConfig
	}
	stateRoot, err := userStateRoot(environ, home)
	if err != nil {
		return Paths{}, err
	}
	if err := ensureOwnerDirectory(stateRoot, uid); err != nil {
		return Paths{}, err
	}
	runtimeBase, err := linuxRuntimeBase(environ, uid)
	if err != nil {
		return Paths{}, err
	}
	// Darwin launchd supplies TMPDIR even when the installing shell does not.
	// Keep its per-user socket in the protected state directory so GUI, SSH and
	// launchd clients always use the same namespace.
	var runtimeRoot string
	if runtimeBase == "" {
		runtimeRoot = filepath.Join(stateRoot, "run")
	} else {
		runtimeRoot = filepath.Join(runtimeBase, "paperboat")
	}
	if err := ensureOwnerDirectory(runtimeRoot, uid); err != nil {
		return Paths{}, err
	}
	result := Paths{StateRoot: stateRoot, RuntimeRoot: runtimeRoot, SocketPath: filepath.Join(runtimeRoot, "local-api.sock"), LockPath: filepath.Join(stateRoot, "daemon.lock")}
	if len(result.SocketPath) > maxUnixSocketPath {
		fallback := filepath.Join(stateRoot, "run")
		if err := ensureOwnerDirectory(fallback, uid); err != nil {
			return Paths{}, err
		}
		result.RuntimeRoot, result.SocketPath = fallback, filepath.Join(fallback, "local-api.sock")
	}
	if len(result.SocketPath) > maxUnixSocketPath {
		return Paths{}, ErrInvalidConfig
	}
	return result, nil
}

func userStateRoot(environ func(string) string, home string) (string, error) {
	if runtime.GOOS == "linux" {
		if base := environ("XDG_STATE_HOME"); base != "" {
			if !filepath.IsAbs(base) {
				return "", ErrInvalidConfig
			}
			return filepath.Join(filepath.Clean(base), "paperboat"), nil
		}
		return filepath.Join(home, ".local", "state", "paperboat"), nil
	}
	return filepath.Join(home, "Library", "Application Support", "Paperboat", "state"), nil
}

// linuxRuntimeBase is the user's runtime directory for the socket, or "" for the state directory.
// root gets "": its daemon is a system unit (service.UserServiceRoot) started at boot, while
// /run/user/0 exists only during a login session, so daemon and CLI could resolve different sockets.
func linuxRuntimeBase(environ func(string) string, uid int) (string, error) {
	if runtime.GOOS != "linux" || uid == 0 {
		return "", nil
	}
	candidate := environ("XDG_RUNTIME_DIR")
	if candidate == "" {
		candidate = filepath.Join("/run/user", strconv.Itoa(uid))
	} else if !filepath.IsAbs(candidate) {
		return "", ErrInvalidConfig
	}
	if safeOwnerDirectory(candidate, uid) {
		return candidate, nil
	}
	return "", nil
}

func ensureOwnerDirectory(path string, uid int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidConfig
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || fileOwner(info) != uid || info.Mode().Perm()&0o077 != 0 {
		if err != nil {
			return err
		}
		return ErrUnsafeSocket
	}
	if err := ownerpath.CheckAncestors(path, uid); err != nil {
		return errors.Join(ErrUnsafeSocket, err)
	}
	return nil
}

func safeOwnerDirectory(path string, uid int) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && fileOwner(info) == uid && info.Mode().Perm()&0o022 == 0
}
