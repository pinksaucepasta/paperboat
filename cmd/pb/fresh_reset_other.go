//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/endpointbinary"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/spf13/cobra"
)

func freshEnrollmentRuntimePurge(command *cobra.Command) error { return purgePlatformRuntime(command) }

func existingFreshEnrollmentExecutable() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path, err := endpointbinary.CLI(filepath.Join(home, ".local", "bin", "pb"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, err := installsource.Inspect(path, "resume", installsource.Custom); err != nil {
		return "", err
	}
	return path, nil
}
