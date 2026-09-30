//go:build linux || darwin

package deviceguard

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func systemHostsPath() (string, error) { return "/etc/hosts", nil }
func flushHostsCache() {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 5e9)
		defer cancel()
		_ = exec.CommandContext(ctx, "/usr/bin/dscacheutil", "-flushcache").Run()
	}
}
func replaceHostsFile(ctx context.Context, path string, info os.FileInfo, original, next []byte) error {
	attributes, err := hostsAttributes(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	file, err := os.CreateTemp(parent, ".paperboat-hosts-")
	if err != nil {
		return err
	}
	staged := file.Name()
	file.Close()
	defer os.Remove(staged)
	// Native cp preserves ACLs as well as mode/owner; Linux also preserves xattrs.
	option := "-p"
	if runtime.GOOS == "linux" {
		option = "--preserve=all"
	}
	if err = exec.CommandContext(ctx, "/bin/cp", option, path, staged).Run(); err != nil {
		return err
	}
	file, err = os.OpenFile(staged, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(next)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) || !bytes.Equal(actual, original) {
		return errors.New("hosts file changed during update; left unchanged; retry")
	}
	copied, err := os.Stat(staged)
	if err != nil {
		return err
	}
	before := info.Sys().(*syscall.Stat_t)
	after := copied.Sys().(*syscall.Stat_t)
	if before.Uid != after.Uid || before.Gid != after.Gid || info.Mode() != copied.Mode() {
		return errors.New("hosts security metadata could not be preserved; left unchanged")
	}
	copiedAttributes, err := hostsAttributes(staged)
	if err != nil {
		return err
	}
	currentAttributes, err := hostsAttributes(path)
	if err != nil {
		return err
	}
	if !maps.Equal(attributes, copiedAttributes) || !maps.Equal(attributes, currentAttributes) {
		return errors.New("hosts ACL or extended attributes changed or could not be preserved; left unchanged")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(staged, path); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func hostsAttributes(path string) (map[string]string, error) {
	size, err := unix.Listxattr(path, nil)
	if errors.Is(err, unix.ENOTSUP) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if size > 1<<20 {
		return nil, errors.New("hosts extended attributes exceed limit")
	}
	buffer := make([]byte, size)
	size, err = unix.Listxattr(path, buffer)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, name := range strings.Split(string(buffer[:size]), "\x00") {
		if name == "" {
			continue
		}
		length, err := unix.Getxattr(path, name, nil)
		if err != nil {
			return nil, err
		}
		if length > 1<<20 {
			return nil, errors.New("hosts extended attribute exceeds limit")
		}
		value := make([]byte, length)
		length, err = unix.Getxattr(path, name, value)
		if err != nil {
			return nil, err
		}
		values[name] = string(value[:length])
	}
	return values, nil
}
