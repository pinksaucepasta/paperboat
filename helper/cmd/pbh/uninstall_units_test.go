package main

import (
	"context"
	"errors"
	"fmt"
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

// A reload that failed after the unit files were deleted is retried on the next run on a systemd host, and a
// host without systemd (--no-service) is never asked to reload.
func TestReloadSystemdRetriesWhereSystemdRunsAndSkipsWhereItDoesNot(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"systemctl daemon-reload": ""}
	runtime := t.TempDir()
	if err := reloadSystemd(t.Context(), f.run, false, runtime); err != nil || f.called("systemctl daemon-reload") != 1 {
		t.Fatalf("rerun on a systemd host: err %v calls %v", err, f.calls)
	}
	f.calls = nil
	if err := reloadSystemd(t.Context(), f.run, false, filepath.Join(runtime, "absent")); err != nil || len(f.calls) != 0 {
		t.Fatalf("no-service host: err %v calls %v", err, f.calls)
	}
	if err := reloadSystemd(t.Context(), f.run, true, filepath.Join(runtime, "absent")); err != nil || f.called("systemctl daemon-reload") != 1 {
		t.Fatalf("units removed: err %v calls %v", err, f.calls)
	}
	failing := newFakeHost(t, "60000 65535") // reload not answered: fails
	if err := reloadSystemd(t.Context(), failing.run, true, runtime); err == nil || !strings.Contains(err.Error(), "rerun pbh uninstall") {
		t.Fatalf("err %v", err)
	}
}

// The whole uninstall, rerun after a failure at its last systemd step: the first run removes the units and fails to
// reload, the second still reloads (systemd is running), nothing is torn down twice, and every file is gone.
func TestUninstallRetryAfterAFailedReloadAtTheEntryPoint(t *testing.T) {
	root := t.TempDir()
	unitDir, runtimeDir, binaries, binary := filepath.Join(root, "units"), filepath.Join(root, "run"), filepath.Join(root, "lib"), filepath.Join(root, "pbh")
	for _, d := range []string{unitDir, runtimeDir, binaries} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range append([]string{selfhostUnitName}, certificateUnits...) {
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(binary, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakeHost(t, "60000 65535")
	var systemctl []string
	reloadFails := true
	env := uninstallEnv{
		systemctl: func(_ context.Context, name string, args ...string) (string, error) {
			line := strings.Join(append([]string{name}, args...), " ")
			systemctl = append(systemctl, line)
			if line == "systemctl daemon-reload" && reloadFails {
				return "", errors.New("Failed to connect to bus")
			}
			return "", nil
		},
		run: f.run, unitDir: unitDir, certEnv: filepath.Join(root, "none.env"), runtimeDir: runtimeDir,
		paths: f.paths, binaries: binaries, binary: binary,
		teardown: func(context.Context) error { return nil }, removeLocal: func() error { return nil },
	}
	err := runUninstall(t.Context(), env, false, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "rerun pbh uninstall") {
		t.Fatalf("first run: %v", err)
	}
	for _, name := range append([]string{selfhostUnitName}, certificateUnits...) {
		if _, statErr := os.Stat(filepath.Join(unitDir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s survived the first run: %v", name, statErr)
		}
	}
	if _, statErr := os.Stat(binaries); statErr != nil {
		t.Fatalf("binaries removed although the reload failed: %v", statErr)
	}
	systemctl = nil
	reloadFails = false
	var out strings.Builder
	if err := runUninstall(t.Context(), env, false, &out); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if strings.Join(systemctl, "|") != "systemctl daemon-reload" {
		t.Fatalf("retry ran %q, want only the reload", systemctl)
	}
	for _, gone := range []string{binaries, binary} {
		if _, statErr := os.Stat(gone); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s survived the retry: %v", gone, statErr)
		}
	}
	if !strings.Contains(out.String(), "Remove the node from Dashboard") {
		t.Fatalf("output %q", out.String())
	}
}

// The command line: arguments are checked, a non-root run re-executes under sudo and removes nothing itself, and
// with root the real wiring runs the flow; --remove-data (with its confirmation) deletes the local data, a plain
// uninstall keeps it.
func TestUninstallCommandLineWiring(t *testing.T) {
	root := t.TempDir()
	newEnv := func(removed *int, removeErr error) uninstallEnv {
		f := newFakeHost(t, "60000 65535")
		unitDir := filepath.Join(root, "units"+fmt.Sprint(*removed))
		_ = os.MkdirAll(unitDir, 0o755)
		return uninstallEnv{
			systemctl: func(context.Context, string, ...string) (string, error) { return "", nil }, run: f.run,
			unitDir: unitDir, certEnv: filepath.Join(unitDir, "none.env"), runtimeDir: filepath.Join(root, "no-systemd"), paths: f.paths,
			binaries: filepath.Join(root, "lib"), binary: filepath.Join(root, "pbh"),
			teardown: func(context.Context) error { return nil }, removeLocal: func() error { *removed++; return removeErr },
		}
	}
	defer func(r func() bool, e func() uninstallEnv) { isRoot, uninstallEnvFor = r, e }(isRoot, uninstallEnvFor)
	for name, test := range map[string]struct {
		args        []string
		wantRemoved int
		wantErr     string
		removeErr   error
		wantOutput  string
	}{
		"keeps data":             {args: nil, wantOutput: "retained for reinstall"},
		"removes data":           {args: []string{"--remove-data", "--confirm-remove-data"}, wantRemoved: 1, wantOutput: "confirmed local data removed"},
		"needs the confirmation": {args: []string{"--remove-data"}, wantErr: "requires both"},
		"stray argument":         {args: []string{"extra"}, wantErr: "unexpected uninstall arguments"},
		"data removal fails":     {args: []string{"--remove-data", "--confirm-remove-data"}, wantRemoved: 1, removeErr: errors.New("disk error"), wantErr: "disk error"},
	} {
		removed := 0
		isRoot, uninstallEnvFor = func() bool { return true }, func() uninstallEnv { return newEnv(&removed, test.removeErr) }
		var out strings.Builder
		err := uninstall(t.Context(), test.args, &out)
		if (test.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), test.wantErr)) || removed != test.wantRemoved || !strings.Contains(out.String(), test.wantOutput) {
			t.Errorf("%s: err %v removed %d output %q", name, err, removed, out.String())
		}
	}
	// The default wiring points at the real host paths and the real elevation, with nothing missing.
	env := defaultUninstallEnv()
	if env.systemctl == nil || env.run == nil || env.teardown == nil || env.removeLocal == nil || env.unitDir != systemUnitDir || env.certEnv != certificateEnvFile ||
		env.runtimeDir != systemdRuntimeDir || env.binaries != defaultBinaries || env.binary != "/usr/local/bin/pbh" || env.paths != defaultHostPaths(defaultState) {
		t.Fatalf("default env %+v", env)
	}
}
