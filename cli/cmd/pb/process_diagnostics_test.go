package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

func TestTeardownDiagnosticsDoesNotOpenRemovedStorage(t *testing.T) {
	previous := currentLocalDaemonPaths
	t.Cleanup(func() { currentLocalDaemonPaths = previous })
	currentLocalDaemonPaths = func() (localapi.Paths, error) {
		return localapi.Paths{}, errors.New("removed storage")
	}
	for _, args := range [][]string{
		{"uninstall", "--json"},
		{"--no-customization", "uninstall", "--json"},
		{"--config", "private-config", "reset", "--json"},
		{"__runtime-service", "purge"},
		{"__runtime-service", "purge", "--preserve-cli"},
	} {
		t.Run(args[0]+args[1], func(t *testing.T) {
			var stderr bytes.Buffer
			_, closeDiagnostics := openProcessDiagnostics(t.Context(), args, &errorreport.Reporter{}, &stderr)
			closeDiagnostics()
			if stderr.Len() != 0 {
				t.Fatalf("teardown accessed diagnostic storage: %s", stderr.String())
			}
		})
	}
}

// The root update activation helper has no HOME, so it printed "Local
// diagnostics could not be saved" at every update step.
func TestUpdateServiceDiagnosticsUseItsOwnStateRoot(t *testing.T) {
	previous := currentLocalDaemonPaths
	t.Cleanup(func() { currentLocalDaemonPaths = previous })
	currentLocalDaemonPaths = func() (localapi.Paths, error) { return localapi.Paths{}, errors.New("$HOME is not defined") }
	root := t.TempDir()
	t.Setenv("PAPERBOAT_UPDATE_STATE_ROOT", root)
	var stderr bytes.Buffer
	_, closeDiagnostics := openProcessDiagnostics(t.Context(), []string{"daemon", "__runtime-updated", "--activation-helper"}, &errorreport.Reporter{}, &stderr)
	closeDiagnostics()
	if stderr.Len() != 0 {
		t.Fatalf("update helper could not save diagnostics: %s", stderr.String())
	}
	if entries, err := os.ReadDir(filepath.Join(root, "diagnostics")); err != nil || len(entries) == 0 {
		t.Fatalf("no diagnostics under the update state root: %v %v", entries, err)
	}
	stderr.Reset()
	_, closeDiagnostics = openProcessDiagnostics(t.Context(), []string{"status"}, &errorreport.Reporter{}, &stderr)
	closeDiagnostics()
	if stderr.Len() == 0 {
		t.Fatal("ordinary commands must keep using the user's state directory")
	}
}

// The root machine guard runs with no HOME too; its diagnostics belong under
// its own state directory, not the user's.
func TestMachineGuardDiagnosticsUseItsOwnStateDirectory(t *testing.T) {
	if machineguard.DefaultStateDir == "" {
		t.Skip("machine guard is not supported on this platform")
	}
	t.Setenv("HOME", "")
	command, _, err := newRootCommand().Find([]string{"daemon", "machine-guard", "run"})
	if err != nil {
		t.Fatal(err)
	}
	directory, err := processDiagnosticsDirectory(command)
	if err != nil {
		t.Fatalf("machine guard diagnostics need HOME: %v", err)
	}
	if want := filepath.Join(machineguard.DefaultStateDir, "diagnostics"); directory != want {
		t.Fatalf("machine guard diagnostics directory = %q, want %q", directory, want)
	}
}

// lanex2-m 2026-11-16: root `pb daemon machine-guard install` under sudo kept the user's HOME and warned "settings
// could not be saved": root refuses the user-owned diagnostics directory. Root guard commands use the guard's own.
func TestRootMachineGuardInstallDiagnosticsUseGuardStateDirectory(t *testing.T) {
	if machineguard.DefaultStateDir == "" {
		t.Skip("machine guard is not supported on this platform")
	}
	command, _, err := newRootCommand().Find([]string{"daemon", "machine-guard", "install"})
	if err != nil || command.Name() != "install" {
		t.Fatalf("find machine-guard install: %v", err)
	}
	old := processEUID
	t.Cleanup(func() { processEUID = old })
	want := filepath.Join(machineguard.DefaultStateDir, "diagnostics")
	processEUID = func() int { return 1000 }
	if directory, _ := processDiagnosticsDirectory(command); directory == want {
		t.Fatal("a user's machine-guard install must keep the user's diagnostics directory")
	}
	processEUID = func() int { return 0 }
	if directory, err := processDiagnosticsDirectory(command); err != nil || directory != want {
		t.Fatalf("root machine-guard install diagnostics = %q, %v; want %q", directory, err, want)
	}
}

func TestForegroundSupervisorKeepsItsDiagnosticsOnTheStateVolume(t *testing.T) {
	want := hostruntimecmd.ForegroundDiagnosticsDirectory()
	if want == "" {
		t.Skip("the container supervisor is Linux only")
	}
	// The command is found from the real arguments, before its flags are parsed.
	args := []string{"daemon", "--foreground"}
	command, _, err := newRootCommand().Find(args)
	if err != nil {
		t.Fatal(err)
	}
	if got := foregroundDiagnosticsDirectory(command, args); got != want {
		t.Fatalf("foreground diagnostics directory = %q; want %q (root's home is read-only in the image)", got, want)
	}
	if got := foregroundDiagnosticsDirectory(command, []string{"daemon"}); got != "" {
		t.Fatalf("a daemon without --foreground uses %q", got)
	}
}
