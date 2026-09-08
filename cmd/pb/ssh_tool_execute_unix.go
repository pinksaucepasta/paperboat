//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

func executeManagedSSHTool(tool string, arguments, environment []string) error {
	path, err := exec.LookPath(tool)
	if err != nil {
		return fmt.Errorf("%s is not installed or not available on PATH: %w", tool, err)
	}
	argv := append([]string{path}, arguments...)
	if err := syscall.Exec(path, argv, environment); err != nil {
		return fmt.Errorf("start %s: %w", tool, err)
	}
	return errors.New("managed SSH tool unexpectedly returned")
}
