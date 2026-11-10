//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stageInFlightHelper(t *testing.T, state string) string {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, strings.Repeat("ab", 16))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := protectWindowsUninstallPath(directory, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "paperboat-uninstall-helper.exe"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(directory, "status.json")
	if err := writeProtectedWindowsUninstallJSON(statusPath, windowsUninstallStatus{Schema: windowsUninstallStatusSchema, State: state, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	previousRoot := windowsUninstallRoot
	windowsUninstallRoot = func() string { return root }
	t.Cleanup(func() { windowsUninstallRoot = previousRoot })
	return statusPath
}

// pb install right after pb uninstall (the helper still removing) failed local_io inside its elevated step.
func TestInstallRefusesWhileAnUninstallHelperIsStillRemoving(t *testing.T) {
	statusPath := stageInFlightHelper(t, "removing")
	previous := windowsHelperIsRunning
	t.Cleanup(func() { windowsHelperIsRunning = previous })
	windowsHelperIsRunning = func(string) (bool, error) { return true, nil }
	err := windowsUninstallInFlight()
	if err == nil {
		t.Fatal("an install was allowed while the uninstall helper is removing")
	}
	for _, want := range []string{"still being removed", "pb install", "about a minute", statusPath} {
		if !strings.Contains(userFacingError(err), want) {
			t.Fatalf("the refusal lacks %q: %q", want, userFacingError(err))
		}
	}
	windowsHelperIsRunning = func(string) (bool, error) { return false, nil }
	if err := windowsUninstallInFlight(); err != nil {
		t.Fatalf("a helper that is gone blocked the install: %v", err)
	}
	completed := stageInFlightHelper(t, "completed")
	windowsHelperIsRunning = func(string) (bool, error) { return true, nil }
	if err := windowsUninstallInFlight(); err != nil {
		t.Fatalf("a completed uninstall (%s) blocked the install: %v", completed, err)
	}
}
