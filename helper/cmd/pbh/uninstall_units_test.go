package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Uninstall deletes the directory edge-ip-certificate.py lives in, so the timer and service that run it must go
// with it (disabled first); unrelated units stay, and the env file goes only with the confirmed data removal.
func TestUninstallDisablesAndRemovesTheCertificateTimerAndService(t *testing.T) {
	for _, removeData := range []bool{false, true} {
		dir := t.TempDir()
		env := filepath.Join(dir, "paperboat-edge-certificate.env")
		other := filepath.Join(dir, "unrelated.service")
		for _, name := range append([]string{env, other}, filepath.Join(dir, certificateUnits[0]), filepath.Join(dir, certificateUnits[1])) {
			if err := os.WriteFile(name, []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		f := newFakeHost(t, "60000 65535")
		f.answers = map[string]string{"systemctl disable --now " + certificateUnits[0]: "", "systemctl disable --now " + certificateUnits[1]: ""}
		if err := removeCertificateUnits(t.Context(), f.run, dir, env, removeData); err != nil {
			t.Fatal(err)
		}
		for _, unit := range certificateUnits {
			if f.called("systemctl disable --now "+unit) != 1 {
				t.Errorf("%s not disabled: %v", unit, f.calls)
			}
			if _, err := os.Stat(filepath.Join(dir, unit)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s survived uninstall: %v", unit, err)
			}
		}
		if _, err := os.Stat(other); err != nil {
			t.Errorf("an unrelated unit was removed: %v", err)
		}
		if _, err := os.Stat(env); errors.Is(err, os.ErrNotExist) != removeData {
			t.Errorf("env file removed = %v, want %v", errors.Is(err, os.ErrNotExist), removeData)
		}
	}
	// Nothing installed: nothing to disable.
	f := newFakeHost(t, "60000 65535")
	if err := removeCertificateUnits(t.Context(), f.run, t.TempDir(), filepath.Join(t.TempDir(), "none.env"), true); err != nil || len(f.calls) != 0 {
		t.Fatalf("err %v calls %v", err, f.calls)
	}
}

// A unit that cannot be disabled is reported and stays on disk, so a rerun finds it again.
func TestUninstallKeepsACertificateUnitItCouldNotDisable(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, certificateUnits[0])
	if err := os.WriteFile(unit, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeHost(t, "60000 65535") // systemctl is not answered: the disable fails
	err := removeCertificateUnits(t.Context(), f.run, dir, filepath.Join(dir, "none.env"), false)
	if err == nil || !strings.Contains(err.Error(), "rerun pbh uninstall") {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(unit); statErr != nil {
		t.Fatalf("the unit was removed although disable failed: %v", statErr)
	}
	// The env file the unit reads stays until the unit is gone, even for the confirmed data removal.
	env := filepath.Join(dir, "paperboat-edge-certificate.env")
	if err := os.WriteFile(env, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeCertificateUnits(t.Context(), f.run, dir, env, true); err == nil {
		t.Fatal("second failure not reported")
	}
	if _, statErr := os.Stat(env); statErr != nil {
		t.Fatalf("env removed while its unit still exists: %v", statErr)
	}
}
