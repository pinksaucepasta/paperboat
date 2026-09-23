//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

const manualRefreshTimeout = 30 * time.Second

var errManualRefreshPending = errors.New("manual refresh remains pending; check the manual directory permissions and available disk space, then retry pb update")

// The caller authenticates the canonical executable against the committed
// release before invoking this closure. Never use the updater's embedded pages:
// the persistent activation helper intentionally runs the previous release.
func newUnixManualRefresher(binary string, uid, gid int) (func(context.Context) error, error) {
	layout, err := service.UserLayout(runtime.GOOS, uid)
	if err != nil || binary != layout.Binary || uid < 0 || gid < 0 || (uid == 0) != (gid == 0) {
		return nil, updated.ErrInvalidConfig
	}
	return func(ctx context.Context) error {
		if ctx == nil {
			return updated.ErrInvalidConfig
		}
		owner, err := hostinstall.LoadEnrolledOwner(uid)
		if err != nil || owner.UID != uid || owner.GID != gid {
			return errManualRefreshPending
		}
		if runtime.GOOS == "darwin" {
			if os.Geteuid() != 0 || prepareSystemManualDirectory("/usr/local/share/man/man1") != nil {
				return errManualRefreshPending
			}
		}
		return runUnixManualRefresh(ctx, binary, runtime.GOOS, owner)
	}, nil
}

func runUnixManualRefresh(ctx context.Context, binary, platform string, owner hostinstall.Request) error {
	if ctx == nil {
		return updated.ErrInvalidConfig
	}
	childCtx, cancel := context.WithTimeout(ctx, manualRefreshTimeout)
	defer cancel()
	command, err := unixManualRefreshCommand(childCtx, binary, platform, owner)
	if err != nil {
		return err
	}
	if err := command.Run(); err != nil {
		// Child diagnostics can contain user-controlled paths/content. Preserve only
		// cancellation identity and the actionable fixed recovery message.
		return errors.Join(errManualRefreshPending, childCtx.Err())
	}
	return nil
}

func unixManualRefreshCommand(ctx context.Context, binary, platform string, owner hostinstall.Request) (*exec.Cmd, error) {
	if ctx == nil || !filepath.IsAbs(binary) || !filepath.IsAbs(owner.Home) || owner.UID < 0 || owner.GID < 0 || (owner.UID == 0) != (owner.GID == 0) {
		return nil, updated.ErrInvalidConfig
	}
	directory := filepath.Join(owner.Home, ".local", "share", "man")
	home, username := owner.Home, owner.User
	credential := &syscall.Credential{Uid: uint32(owner.UID), Gid: uint32(owner.GID), Groups: []uint32{uint32(owner.GID)}}
	switch platform {
	case "linux":
	case "darwin":
		directory, home, username = "/usr/local/share/man", "/var/root", "root"
		credential = &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{0}}
	default:
		return nil, updated.ErrInvalidConfig
	}
	command := exec.CommandContext(ctx, binary, "--no-customization", "__man-pages", "--directory", directory)
	command.Env = []string{"HOME=" + home, "USER=" + username, "LOGNAME=" + username, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: credential}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command, nil
}

// Every ancestor is inspected before creating the next component. Once a
// parent is root-owned and not writable by other users, they cannot replace
// its child between inspection and mkdir. Never repair an unsafe existing path.
func prepareSystemManualDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errManualRefreshPending
	}
	current := string(filepath.Separator)
	components := append([]string{""}, strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator))...)
	for _, component := range components {
		if component != "" {
			current = filepath.Join(current, component)
		}
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(current, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return errManualRefreshPending
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errManualRefreshPending
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return errManualRefreshPending
		}
	}
	return nil
}
