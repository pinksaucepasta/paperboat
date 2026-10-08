//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGuardInstallationRollbackRestoresBeforeRestart(t *testing.T) {
	dir := t.TempDir()
	helper, unit := filepath.Join(dir, "pb"), filepath.Join(dir, "service")
	for _, path := range []string{helper, unit} {
		if err := os.WriteFile(path, []byte("prior"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := snapshotGuardFilesValidated(func(string, string, os.FileInfo) error { return nil }, helper, unit)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{helper, unit} {
		replacement := path + ".new"
		if err := os.WriteFile(replacement, []byte("candidate"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := replaceStateFile(replacement, path); err != nil {
			t.Fatal(err)
		}
		r.record(path)
	}
	failure := errors.New("candidate not ready")
	restarted := false
	finishGuardInstallation(context.Background(), r, &failure, func(context.Context) error { return nil }, func(context.Context) error {
		for _, path := range []string{helper, unit} {
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "prior" {
				t.Fatalf("restart before prior bytes restored: %q %v", data, err)
			}
		}
		restarted = true
		return nil
	})
	if !restarted {
		t.Fatal("prior installation not restarted")
	}
	copies, err := filepath.Glob(filepath.Join(dir, ".paperboat-guard-prior-*"))
	if err != nil || len(copies) != 0 {
		t.Fatalf("rollback copies leaked: %v %v", copies, err)
	}
}

func TestGuardInstallationRollbackPreservesExternalReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pb")
	if err := os.WriteFile(path, []byte("prior"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := snapshotGuardFilesValidated(func(string, string, os.FileInfo) error { return nil }, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.cleanup()
	candidate := path + ".candidate"
	if err := os.WriteFile(candidate, []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replaceStateFile(candidate, path); err != nil {
		t.Fatal(err)
	}
	r.record(path)
	foreign := path + ".foreign"
	if err := os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replaceStateFile(foreign, path); err != nil {
		t.Fatal(err)
	}
	if err := r.restore(); err == nil {
		t.Fatal("external replacement accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "foreign" {
		t.Fatalf("external replacement changed: %q %v", data, err)
	}
	if _, err := os.Stat(r.files[0].backup); err != nil {
		t.Fatalf("prior bytes not retained for recovery: %v", err)
	}
}
