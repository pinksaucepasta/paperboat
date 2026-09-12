//go:build !windows

package windowsopenssh

import (
	"context"
	"path/filepath"
	"strings"
)

func collectLoopbackHealth(context.Context, Config, Result) (ServiceHealth, error) {
	return ServiceHealth{}, ErrInstallerUnavailable
}

func validLoopbackServiceCommand(config Config, _ Result, command string) bool {
	return strings.Contains(strings.ToLower(command), strings.ToLower(filepath.Clean(config.ServiceExecutable)))
}
