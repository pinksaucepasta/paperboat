//go:build linux

package machineguard

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type linuxPurgeFixture struct {
	paths        linuxPurgePaths
	tableExists  bool
	uninstallErr error
	runErrs      map[string]error
	calls        []string
	uninstalls   int
	localNames   int
}

func newLinuxPurgeFixture(t *testing.T) *linuxPurgeFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &linuxPurgeFixture{
		paths: linuxPurgePaths{
			UnitDir:    filepath.Join(root, "units"),
			StateDir:   filepath.Join(root, "state"),
			RuntimeDir: filepath.Join(root, "run"),
			Libexec:    []string{filepath.Join(root, "libexec", "paperboat-machineguard"), filepath.Join(root, "libexec", "paperboat-deviceguard")},
			TrustDirs:  []string{filepath.Join(root, "ca-certificates"), filepath.Join(root, "ssl-certs")},
		},
		runErrs: map[string]error{},
	}
	// The state directory is created only by tests that plant state, because its
	// presence selects the guard uninstall path.
	for _, directory := range append([]string{fixture.paths.UnitDir, fixture.paths.RuntimeDir}, append(fixture.paths.Libexec, fixture.paths.TrustDirs...)...) {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func (f *linuxPurgeFixture) purge(ctx context.Context) error {
	return purgeLinux(ctx, linuxPurge{
		Paths: f.paths,
		Run: func(_ context.Context, name string, args ...string) error {
			command := strings.Join(append([]string{name}, args...), " ")
			f.calls = append(f.calls, command)
			if name == "nft" && slices.Equal(args, []string{"list", "table", "inet", guardTable}) && !f.tableExists {
				return errors.New("no such table")
			}
			return f.runErrs[command]
		},
		Uninstall: func(context.Context) error {
			f.uninstalls++
			return f.uninstallErr
		},
		LocalNames: func(context.Context) error {
			f.localNames++
			return nil
		},
	})
}

func (f *linuxPurgeFixture) count(command string) int {
	total := 0
	for _, call := range f.calls {
		if call == command {
			total++
		}
	}
	return total
}

func writeLinuxFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func linuxAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still exists (err=%v)", path, err)
	}
}

func linuxPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s was removed: %v", path, err)
	}
}

func TestPurgeLinuxRemovesEveryGuardArtifact(t *testing.T) {
	f := newLinuxPurgeFixture(t)
	f.tableExists = true
	units := f.paths.UnitDir
	service := filepath.Join(units, "paperboat-machineguard.service")
	deny := filepath.Join(units, "paperboat-machineguard-deny.service")
	writeLinuxFile(t, service, guardUnitMarker+"[Service]\n")
	writeLinuxFile(t, deny, guardUnitMarker+"[Service]\n")
	wantsDirectory := filepath.Join(units, "multi-user.target.wants")
	requiresDirectory := filepath.Join(units, "systemd-user-sessions.service.requires")
	for _, directory := range []string{wantsDirectory, requiresDirectory} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(service, filepath.Join(wantsDirectory, "paperboat-machineguard.service")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(deny, filepath.Join(requiresDirectory, "paperboat-machineguard-deny.service")); err != nil {
		t.Fatal(err)
	}
	unrelatedUnit := filepath.Join(units, "paperboat-console.service")
	writeLinuxFile(t, unrelatedUnit, "[Service]\n")

	for _, executable := range []string{filepath.Join(f.paths.Libexec[0], "pb"), filepath.Join(f.paths.Libexec[1], "tool")} {
		writeLinuxFile(t, executable, "binary")
	}
	writeLinuxFile(t, filepath.Join(f.paths.StateDir, "reservations.json"), `{"ips":{}}`)
	writeLinuxFile(t, filepath.Join(f.paths.StateDir, "lock"), "")

	digest := sha256.Sum256([]byte("pprbt"))
	trustName := fmt.Sprintf("paperboat-machineguard-%x.crt", digest[:12])
	trustFile := filepath.Join(f.paths.TrustDirs[0], trustName)
	writeLinuxFile(t, trustFile, "-----BEGIN CERTIFICATE-----\n")
	if err := os.Symlink(trustFile, filepath.Join(f.paths.TrustDirs[1], trustName)); err != nil {
		t.Fatal(err)
	}
	unrelatedTrust := filepath.Join(f.paths.TrustDirs[0], "corporate-root.crt")
	writeLinuxFile(t, unrelatedTrust, "-----BEGIN CERTIFICATE-----\n")

	if err := f.purge(context.Background()); err != nil {
		t.Fatalf("purge: %v", err)
	}

	for _, path := range []string{service, deny, filepath.Join(wantsDirectory, "paperboat-machineguard.service"), filepath.Join(requiresDirectory, "paperboat-machineguard-deny.service"), trustFile, filepath.Join(f.paths.TrustDirs[1], trustName), f.paths.StateDir, f.paths.Libexec[0], f.paths.Libexec[1]} {
		linuxAbsent(t, path)
	}
	linuxPresent(t, unrelatedUnit)
	linuxPresent(t, unrelatedTrust)
	if f.count("nft delete table inet paperboat_machineguard") != 1 {
		t.Fatalf("nft delete calls = %d, want 1; calls=%v", f.count("nft delete table inet paperboat_machineguard"), f.calls)
	}
	if f.count("update-ca-certificates --fresh") != 1 {
		t.Fatalf("update-ca-certificates calls = %d, want 1", f.count("update-ca-certificates --fresh"))
	}
	if f.uninstalls != 1 || f.localNames != 1 {
		t.Fatalf("uninstall calls=%d localNames calls=%d, want 1 and 1", f.uninstalls, f.localNames)
	}
	if f.count("systemctl disable --now paperboat-machineguard.service") != 1 {
		t.Fatalf("service was not stopped; calls=%v", f.calls)
	}
}

func TestPurgeLinuxSkipsNFTAndTrustUpdateWhenNothingExists(t *testing.T) {
	f := newLinuxPurgeFixture(t)
	if err := f.purge(context.Background()); err != nil {
		t.Fatalf("purge of an absent guard: %v", err)
	}
	if f.count("nft delete table inet paperboat_machineguard") != 0 {
		t.Fatalf("nft delete ran without a table; calls=%v", f.calls)
	}
	if f.count("update-ca-certificates --fresh") != 0 {
		t.Fatalf("trust update ran without removed trust files; calls=%v", f.calls)
	}
	if f.uninstalls != 0 {
		t.Fatalf("guard uninstall ran without state")
	}
	if f.localNames != 1 {
		t.Fatalf("local name cleanup must run even when the guard was never installed")
	}
}

func TestPurgeLinuxPreservesForeignUnitAndReportsIt(t *testing.T) {
	f := newLinuxPurgeFixture(t)
	foreign := filepath.Join(f.paths.UnitDir, "paperboat-machineguard.service")
	writeLinuxFile(t, foreign, "[Service]\nExecStart=/opt/other\n")

	err := f.purge(context.Background())
	if err == nil || !strings.Contains(err.Error(), "foreign machine guard unit") {
		t.Fatalf("purge error = %v, want a foreign unit report", err)
	}
	linuxPresent(t, foreign)
}

func TestPurgeLinuxRetainsStateAndKeepsErrorsWhenTrustUpdateFails(t *testing.T) {
	f := newLinuxPurgeFixture(t)
	writeLinuxFile(t, filepath.Join(f.paths.StateDir, "reservations.json"), `{"ips":{}}`)
	digest := sha256.Sum256([]byte("pprbt"))
	trustFile := filepath.Join(f.paths.TrustDirs[0], fmt.Sprintf("paperboat-machineguard-%x.crt", digest[:12]))
	writeLinuxFile(t, trustFile, "-----BEGIN CERTIFICATE-----\n")
	trustErr := errors.New("update-ca-certificates interrupted")
	f.runErrs["update-ca-certificates --fresh"] = trustErr
	uninstallErr := errors.New("guard service did not stop")
	f.uninstallErr = uninstallErr

	err := f.purge(context.Background())
	if !errors.Is(err, trustErr) || !errors.Is(err, uninstallErr) {
		t.Fatalf("purge error = %v, want both the trust and uninstall failures", err)
	}
	linuxAbsent(t, trustFile)
	linuxPresent(t, f.paths.StateDir)
	if f.localNames != 1 {
		t.Fatalf("local name cleanup did not run after earlier failures")
	}
	if !strings.Contains(err.Error(), "retained") {
		t.Fatalf("purge error %q does not say that state was retained", err)
	}
}

func TestLinuxPurgeRemovesTheVolatileControlDirectory(t *testing.T) {
	f := newLinuxPurgeFixture(t)
	if err := os.WriteFile(filepath.Join(f.paths.RuntimeDir, "control.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.paths.RuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("control directory survived purge: %v", err)
	}
}
