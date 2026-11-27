//go:build !unix

package main

import "github.com/spf13/cobra"

// Only the Unix install path leaves a privileged step running after pb exits (update_staged_cleanup_unix.go).
var startStagedCleanup = func(string) {}

func stagedCleanupCobraCommand() *cobra.Command { return nil }
