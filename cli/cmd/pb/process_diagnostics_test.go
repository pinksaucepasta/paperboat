package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
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
