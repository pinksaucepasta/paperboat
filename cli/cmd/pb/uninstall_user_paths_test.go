package main

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// a Mac test VM 2026-11-13: pb uninstall left ~/Library/Logs/Paperboat (the per-user daemon's log) behind.
func TestUninstallRemovesTheDaemonLogFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows: os.UserHomeDir reads USERPROFILE
	command := &cobra.Command{}
	command.Flags().String("config", "", "")
	command.Flags().String("state-root", "", "")
	paths, err := userPaperboatRemovalPaths(command)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Library", "Logs", "Paperboat")
	for _, path := range paths {
		if path == want {
			return
		}
	}
	t.Fatalf("%s is not removed on uninstall: %v", want, paths)
}
