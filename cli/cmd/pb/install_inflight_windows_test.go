//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stageInFlightHelper returns the staged status path and a context whose uninstall root is the staged directory;
// helperRunning is what the process query then answers.
func stageInFlightHelper(t *testing.T, state string, helperRunning *bool) (string, context.Context) {
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
	ctx := withWindowsUninstallDependencies(t.Context(), func(d *windowsUninstallDependencies) {
		d.root = func() string { return root }
		d.helperIsRunning = func(string) (bool, error) { return *helperRunning, nil }
	})
	return statusPath, ctx
}

// pb install right after pb uninstall (the helper still removing) failed local_io inside its elevated step.
func TestInstallRefusesWhileAnUninstallHelperIsStillRemoving(t *testing.T) {
	running := true
	statusPath, ctx := stageInFlightHelper(t, "removing", &running)
	err := windowsUninstallInFlight(ctx)
	if err == nil {
		t.Fatal("an install was allowed while the uninstall helper is removing")
	}
	for _, want := range []string{"still being removed", "pb install", "about a minute", statusPath} {
		if !strings.Contains(userFacingError(err), want) {
			t.Fatalf("the refusal lacks %q: %q", want, userFacingError(err))
		}
	}
	running = false
	if err := windowsUninstallInFlight(ctx); err != nil {
		t.Fatalf("a helper that is gone blocked the install: %v", err)
	}
	running = true
	completed, completedCtx := stageInFlightHelper(t, "completed", &running)
	if err := windowsUninstallInFlight(completedCtx); err != nil {
		t.Fatalf("a completed uninstall (%s) blocked the install: %v", completed, err)
	}
}
