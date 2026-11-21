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
		if touched, err := removeCertificateUnits(t.Context(), f.run, dir, env, removeData); err != nil || !touched {
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
	if touched, err := removeCertificateUnits(t.Context(), f.run, t.TempDir(), filepath.Join(t.TempDir(), "none.env"), true); err != nil || touched || len(f.calls) != 0 {
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
	_, err := removeCertificateUnits(t.Context(), f.run, dir, filepath.Join(dir, "none.env"), false)
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
	if _, err := removeCertificateUnits(t.Context(), f.run, dir, env, true); err == nil {
		t.Fatal("second failure not reported")
	}
	if _, statErr := os.Stat(env); statErr != nil {
		t.Fatalf("env removed while its unit still exists: %v", statErr)
	}
}

// Certificate renewal restarts the service, so it is stopped first; a host without units (--no-service, or a
// rerun after they were removed) has nothing to stop and nothing to reload.
func TestUninstallStopsRenewalBeforeTheServiceAndSkipsAbsentUnits(t *testing.T) {
	dir := t.TempDir()
	for _, name := range append([]string{selfhostUnitName}, certificateUnits...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"systemctl disable --now " + certificateUnits[0]: "", "systemctl disable --now " + certificateUnits[1]: "", "systemctl stop " + selfhostUnitName: "", "systemctl disable " + selfhostUnitName: ""}
	touched, err := stopServices(t.Context(), f.run, dir, filepath.Join(dir, "none.env"), false)
	if err != nil || !touched {
		t.Fatalf("touched %v err %v", touched, err)
	}
	want := []string{"systemctl disable --now " + certificateUnits[0], "systemctl disable --now " + certificateUnits[1], "systemctl stop " + selfhostUnitName}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want renewal stopped before the service: %q", f.calls, want)
	}
	if removed, err := removeMainUnit(t.Context(), f.run, dir); err != nil || !removed {
		t.Fatalf("removed %v err %v", removed, err)
	}
	// Rerun: every unit is gone, nothing runs and nothing needs a reload.
	f.calls = nil
	touched, err = stopServices(t.Context(), f.run, dir, filepath.Join(dir, "none.env"), false)
	removed, err2 := removeMainUnit(t.Context(), f.run, dir)
	if err != nil || err2 != nil || touched || removed || len(f.calls) != 0 {
		t.Fatalf("rerun: touched %v removed %v errs %v %v calls %v", touched, removed, err, err2, f.calls)
	}
}

// A stat failure other than "absent" stops uninstall instead of being read as nothing to do.
func TestUninstallTreatsAStatFailureAsAnError(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeHost(t, "60000 65535")
	if _, err := stopServices(t.Context(), f.run, notADir, "", false); err == nil {
		t.Fatal("a stat failure (the unit directory is a file) was treated as no units")
	}
}
