package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// PB-U1 on dev-0a637d078: pb uninstall left $XDG_RUNTIME_DIR/paperboat (managed-SSH known_hosts) behind.
// Resolving what to delete must not create the roots it then deletes.
func TestUninstallRemovesTheDaemonRuntimeRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux keeps the runtime root outside the state root")
	}
	home, runtimeDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	command := &cobra.Command{}
	command.Flags().String("config", "", "")
	command.Flags().String("state-root", "", "")
	root := filepath.Join(runtimeDir, "paperboat")
	paths, err := userPaperboatRemovalPaths(command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) || slices.Contains(paths, root) {
		t.Fatalf("resolving removal paths created or listed a missing runtime root: %v %v", err, paths)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if paths, err = userPaperboatRemovalPaths(command); err != nil || !slices.Contains(paths, root) {
		t.Fatalf("%s is not removed on uninstall: %v %v", root, paths, err)
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
		{"Windows elevated helper", runtimeService, []string{"__runtime-service", "bridge", "--request", "r"}, "windows", true},
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
