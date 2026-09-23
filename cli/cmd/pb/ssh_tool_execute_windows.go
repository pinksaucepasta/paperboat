//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func executeManagedSSHTool(tool string, arguments, environment []string) error {
	path, err := exec.LookPath(tool)
	if err != nil {
		return fmt.Errorf("%s is not installed or not available on PATH: %w", tool, err)
	}
	command := exec.Command(path, arguments...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Env = append([]string(nil), environment...)
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}
