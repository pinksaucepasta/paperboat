//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installedGuardFixture replaces an existing helper with a candidate and records
// the rollback snapshot, the same sequence the platform installers perform.
func installedGuardFixture(t *testing.T) (string, *guardFileRollback) {
	t.Helper()
	dir := t.TempDir()
	helper := filepath.Join(dir, "pb")
	if err := os.WriteFile(helper, []byte("prior"), 0600); err != nil {
		t.Fatal(err)
	}
	rollback, err := snapshotGuardFilesValidated(func(string, string, os.FileInfo) error { return nil }, helper)
	if err != nil {
		t.Fatal(err)
	}
	candidate := helper + ".new"
	if err := os.WriteFile(candidate, []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replaceStateFile(candidate, helper); err != nil {
		t.Fatal(err)
	}
	rollback.record(helper)
	return helper, rollback
}

func TestPendingTrustKeepsInstalledGuardWithoutRollback(t *testing.T) {
	helper, rollback := installedGuardFixture(t)
	result := trustPending(t.Context(), errors.New("The authorization was denied since no user interaction was possible."))
	stops, restores := 0, 0
	finishGuardInstall(t.Context(), rollback, &result, func(context.Context) error { stops++; return nil }, func(context.Context) error { restores++; return nil })
	if !errors.Is(result, ErrTrustPending) {
		t.Fatalf("pending trust result lost its type: %v", result)
	}
	if stops != 0 || restores != 0 {
		t.Fatalf("pending trust rolled back the guard: stops=%d restores=%d", stops, restores)
	}
	if data, err := os.ReadFile(helper); err != nil || string(data) != "candidate" {
		t.Fatalf("installed guard was replaced: %q, %v", data, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(helper), ".paperboat-guard-prior-*")); len(leftovers) != 0 {
		t.Fatalf("rollback copies retained after successful installation: %v", leftovers)
	}
}

func TestRealInstallFailureStillRollsBack(t *testing.T) {
	helper, rollback := installedGuardFixture(t)
	result := errors.New("candidate not ready")
	stops, restores := 0, 0
	finishGuardInstall(t.Context(), rollback, &result, func(context.Context) error { stops++; return nil }, func(context.Context) error { restores++; return nil })
	if errors.Is(result, ErrTrustPending) {
		t.Fatal("real failure reported as pending trust")
	}
	if stops != 1 || restores != 1 {
		t.Fatalf("real failure did not roll back: stops=%d restores=%d", stops, restores)
	}
	if data, err := os.ReadFile(helper); err != nil || string(data) != "prior" {
		t.Fatalf("prior guard not restored: %q, %v", data, err)
	}
}

func TestTrustPendingClassification(t *testing.T) {
	if err := trustPending(t.Context(), nil); err != nil {
		t.Fatalf("successful trust became %v", err)
	}
	cause := "The authorization was denied since no user interaction was possible."
	err := trustPending(t.Context(), errors.New(cause))
	if !errors.Is(err, ErrTrustPending) || !strings.Contains(err.Error(), cause) {
		t.Fatalf("pending trust does not keep its cause: %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	err = trustPending(canceled, errors.New(cause))
	if errors.Is(err, ErrTrustPending) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled trust step was classified as pending: %v", err)
	}
}
