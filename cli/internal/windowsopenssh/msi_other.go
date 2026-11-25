//go:build !windows

package windowsopenssh

import (
	"os"
	"path/filepath"
)

var createProtectedDirectory = func(path string) error { return os.MkdirAll(path, 0o700) }

func msiexecPath() (string, error) { return "msiexec.exe", nil }

var msiDirectory = func() (string, error) { return filepath.Join(os.TempDir(), "paperboat-openssh-installer"), nil }
