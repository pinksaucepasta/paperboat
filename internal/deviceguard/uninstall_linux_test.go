//go:build linux

package deviceguard

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

func TestUninstallLinuxRetainsDenyAndRetriesTrustRemoval(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned filesystem fixtures; run this exact test under sudo")
	}
	root := t.TempDir()
	units := filepath.Join(root, "units")
	state := filepath.Join(root, "state")
	trust := filepath.Join(root, "trust")
	for _, path := range []string{units, state, trust} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeGuardUnit(filepath.Join(units, "paperboat-deviceguard.service"), "[Service]\n"); err != nil {
		t.Fatal(err)
	}
	journal := []byte(`{"ips":{"127.100.0.2":"1001"},"names":{"office.pprbt":"1001"}}`)
	if err := os.WriteFile(filepath.Join(state, "reservations.json"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	ca, err := splitdns.LoadOrCreateConstrainedCA(filepath.Join(state, "certificates", "pprbt"), "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("pprbt"))
	trustPath := filepath.Join(trust, fmt.Sprintf("paperboat-deviceguard-%x.crt", digest[:12]))
	if err = os.WriteFile(trustPath, ca.CertPEM(), 0644); err != nil {
		t.Fatal(err)
	}
	failTrust := true
	var actions []string
	denies := 0
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		actions = append(actions, name+" "+strings.Join(args, " "))
		if name == "update-ca-certificates" && failTrust {
			return nil, errors.New("interrupted trust update")
		}
		return nil, nil
	}
	deny := func(context.Context) error { denies++; return nil }
	resolver := func(context.Context, Config) error {
		if denies == 0 {
			t.Fatal("resolver removed before deny")
		}
		return nil
	}
	result, err := uninstallLinux(t.Context(), units, state, trust, run, deny, resolver)
	if err == nil || !strings.Contains(err.Error(), "retry pb daemon device-guard uninstall") {
		t.Fatalf("partial failure=%v", err)
	}
	if len(result.Retained) == 0 {
		t.Fatal("partial removal hid retained safety state")
	}
	if _, err = os.Stat(trustPath); !os.IsNotExist(err) {
		t.Fatal("owned trust source not removed")
	}
	if _, err = os.Stat(filepath.Join(units, "paperboat-deviceguard.service")); err != nil {
		t.Fatal("recovery ownership record removed before completion")
	}
	failTrust = false
	result, err = uninstallLinux(t.Context(), units, state, trust, run, deny, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 3 || len(result.Retained) == 0 {
		t.Fatalf("result=%+v", result)
	}
	if _, err = os.Stat(filepath.Join(units, "paperboat-deviceguard.service")); !os.IsNotExist(err) {
		t.Fatal("access unit remains")
	}
	if err = validateGuardUnit(filepath.Join(units, "paperboat-deviceguard-deny.service")); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(state, "reservations.json"))
	if err != nil || string(saved) != string(journal) {
		t.Fatal("reservation tombstones changed")
	}
	if _, err = os.Stat(filepath.Join(state, "certificates", "pprbt", "rootCA-key.pem")); err != nil {
		t.Fatal("reinstall CA identity lost")
	}
	if _, err = uninstallLinux(t.Context(), units, state, trust, run, deny, resolver); err != nil {
		t.Fatalf("repeat uninstall: %v", err)
	}
	for _, action := range actions {
		if strings.Contains(action, "disable") && strings.Contains(action, "deny") {
			t.Fatal("disabled safety unit")
		}
	}
}
func TestUninstallLinuxRefusesForeignUnitBeforeMutation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned fixture")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "paperboat-deviceguard.service"), []byte("foreign"), 0644); err != nil {
		t.Fatal(err)
	}
	called := false
	run := func(context.Context, string, ...string) ([]byte, error) { called = true; return nil, nil }
	_, err := uninstallLinux(t.Context(), root, filepath.Join(root, "state"), root, run, func(context.Context) error { called = true; return nil }, func(context.Context, Config) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("foreign ownership err=%v mutation=%v", err, called)
	}
}

// This fixture exercises real systemd lifecycle with unique harmless services.
// It does not mutate host DNS/firewall; separate namespace tests own that proof.
func TestUninstallLinuxSystemdLifecycle(t *testing.T) {
	if os.Getenv("PAPERBOAT_UNINSTALL_SYSTEMD_E2E") != "1" {
		t.Skip("approved systemd target only")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	trust := filepath.Join(root, "trust")
	for _, path := range []string{state, trust} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	base := fmt.Sprintf("paperboat-uninstall-review-%d", os.Getpid())
	cfg := linuxUninstallConfig{UnitDir: "/etc/systemd/system", StateDir: state, TrustDir: trust, ServiceName: base + ".service", DenyName: base + "-deny.service", DenyUnit: "[Unit]\nDescription=Paperboat uninstall lifecycle fixture safety unit\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\nRemainAfterExit=yes\n[Install]\nWantedBy=multi-user.target\n"}
	paths := []string{filepath.Join(cfg.UnitDir, cfg.ServiceName), filepath.Join(cfg.UnitDir, cfg.DenyName)}
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("fixture unit already exists", path)
		}
	}
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	cleanup := func() {
		for _, name := range []string{cfg.ServiceName, cfg.DenyName} {
			_ = exec.Command("systemctl", "disable", "--now", name).Run()
		}
		for _, path := range paths {
			_ = os.Remove(path)
		}
		_ = exec.Command("systemctl", "daemon-reload").Run()
	}
	t.Cleanup(cleanup)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	serviceUnit := "[Unit]\nDescription=Paperboat uninstall lifecycle fixture\n[Service]\nType=simple\nExecStart=/bin/sleep infinity\n[Install]\nWantedBy=multi-user.target\n"
	if err := writeGuardUnit(paths[0], serviceUnit); err != nil {
		t.Fatal(err)
	}
	if err := writeGuardUnit(paths[1], cfg.DenyUnit); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", cfg.ServiceName}} {
		if out, err := run(ctx, "systemctl", args...); err != nil {
			t.Fatalf("fixture setup: %v %s", err, out)
		}
	}
	deny := func(ctx context.Context) error {
		out, err := run(ctx, "systemctl", "start", cfg.DenyName)
		if err != nil {
			return fmt.Errorf("fixture safety activation: %w: %s", err, out)
		}
		return nil
	}
	resolver := func(context.Context, Config) error { return nil }
	original, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths[0], []byte("foreign"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = uninstallLinuxUnits(ctx, cfg, run, deny, resolver); err == nil {
		t.Fatal("foreign unit accepted")
	}
	if err = exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", cfg.ServiceName).Run(); err != nil {
		t.Fatal("foreign unit failure stopped running service")
	}
	if err = os.WriteFile(paths[0], original, 0644); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(state, "certificates", "invalid.public")
	if err = os.MkdirAll(invalid, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = uninstallLinuxUnits(ctx, cfg, run, deny, resolver); err == nil {
		t.Fatal("invalid CA metadata should interrupt cleanup")
	}
	if err = exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", cfg.DenyName).Run(); err != nil {
		t.Fatal("partial failure did not retain active safety service")
	}
	if err = os.Remove(invalid); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, e := uninstallLinuxUnits(ctx, cfg, run, deny, resolver)
		if e != nil || len(result.Retained) == 0 {
			t.Fatalf("uninstall%d=%+v %v", i, result, e)
		}
	}
	if _, err = os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("product service unit retained")
	}
	if err = exec.CommandContext(ctx, "systemctl", "is-enabled", "--quiet", cfg.DenyName).Run(); err != nil {
		t.Fatal("safety boot service disabled")
	}
	if err = exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", cfg.DenyName).Run(); err != nil {
		t.Fatal("safety service stopped")
	}
}
