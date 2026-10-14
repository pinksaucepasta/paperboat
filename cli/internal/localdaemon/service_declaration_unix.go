//go:build darwin || linux

package localdaemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/endpointbinary"
	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"howett.net/plist"
)

// A stopped launchd job is unregistered, but its verified declaration remains
// installed. Never bootstrap arbitrary content merely because a plist exists.
func darwinServiceInstalled(path, executable string, uid int) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != uid || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > 64<<10 {
		return false, hostservice.ErrInvalidDefinition
	}
	body, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return false, err
	}
	if len(body) > 64<<10 {
		return false, hostservice.ErrInvalidDefinition
	}
	var declaration struct {
		Label     string   `plist:"Label"`
		Program   string   `plist:"Program"`
		Arguments []string `plist:"ProgramArguments"`
	}
	if _, err = plist.Unmarshal(body, &declaration); err != nil {
		return false, err
	}
	expected, err := endpointbinary.DaemonPathForRemoval(executable)
	if err != nil {
		return false, err
	}
	if declaration.Label != hostservice.DaemonLabel || declaration.Program != "" || len(declaration.Arguments) < 2 || declaration.Arguments[0] != expected || declaration.Arguments[1] != "daemon" {
		return false, hostservice.ErrInvalidDefinition
	}
	for i := 2; i < len(declaration.Arguments); i += 2 {
		if i+1 >= len(declaration.Arguments) {
			return false, hostservice.ErrInvalidDefinition
		}
		value := declaration.Arguments[i+1]
		switch declaration.Arguments[i] {
		case "--config":
			if !filepath.IsAbs(value) || filepath.Clean(value) != value {
				return false, hostservice.ErrInvalidDefinition
			}
		case "--server":
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
				return false, hostservice.ErrInvalidDefinition
			}
		default:
			return false, hostservice.ErrInvalidDefinition
		}
	}
	return true, nil
}
