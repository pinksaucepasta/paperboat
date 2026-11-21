package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"

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

// PB-U1 on dev-0a637d078: pb uninstall left $XDG_RUNTIME_DIR/paperboat (managed-SSH known_hosts) behind.
func TestUninstallRemovesTheDaemonRuntimeRoot(t *testing.T) {
	home, runtimeDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	command := &cobra.Command{}
	command.Flags().String("config", "", "")
	command.Flags().String("state-root", "", "")
	paths, err := userPaperboatRemovalPaths(command)
	if err != nil {
		t.Fatal(err)
	}
	local, err := localapi.CurrentPaths(os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, local.RuntimeRoot) {
		t.Fatalf("%s is not removed on uninstall: %v", local.RuntimeRoot, paths)
	}
}

// PB-U1 on dev-0a637d078: removal commands keep diagnostics in memory; services keep them on disk.
func TestRemovalCommandsKeepDiagnosticsInMemory(t *testing.T) {
	runtimeService := &cobra.Command{Use: "__runtime-service"}
	cases := []struct {
		name    string
		command *cobra.Command
		args    []string
		goos    string
		want    bool
	}{
		{"purge", runtimeService, []string{"__runtime-service", "purge"}, "linux", true},
		{"purge only as an argument", runtimeService, []string{"__runtime-service", "hostd", "purge"}, "linux", false},
		{"service process", runtimeService, []string{"__runtime-service", "hostd"}, "linux", false},
		{"uninstall", &cobra.Command{Use: "uninstall"}, nil, "linux", true},
		{"complete-uninstall off Windows", &cobra.Command{Use: "__complete-uninstall"}, nil, "linux", false},
		{"complete-uninstall on Windows", &cobra.Command{Use: "__complete-uninstall"}, nil, "windows", true},
		{"status", &cobra.Command{Use: "status"}, nil, "linux", false},
	}
	for _, tc := range cases {
		if got := diagnosticsStayInMemory(tc.command, tc.args, tc.goos); got != tc.want {
			t.Errorf("%s: diagnosticsStayInMemory = %v, want %v", tc.name, got, tc.want)
		}
	}
}
