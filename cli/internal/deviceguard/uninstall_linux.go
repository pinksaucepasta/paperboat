//go:build linux

package deviceguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func Uninstall(ctx context.Context) (UninstallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := requirePrivilege(); err != nil {
		return UninstallResult{}, err
	}
	lifecycle, lockErr := lockGuardLifecycle(DefaultStateDir)
	if lockErr != nil {
		return UninstallResult{}, lockErr
	}
	defer lifecycle.Close()
	return uninstallLinux(ctx, "/etc/systemd/system", DefaultStateDir, "/usr/local/share/ca-certificates", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}, RestoreDeny, removeOwnedResolver)
}

type linuxUninstallConfig struct{ UnitDir, StateDir, TrustDir, ServiceName, DenyName, DenyUnit string }

func uninstallLinux(ctx context.Context, units, state, trust string, run func(context.Context, string, ...string) ([]byte, error), deny func(context.Context) error, removeResolver func(context.Context, Config) error) (result UninstallResult, err error) {
	return uninstallLinuxUnits(ctx, linuxUninstallConfig{units, state, trust, "paperboat-deviceguard.service", "paperboat-deviceguard-deny.service", guardDenyUnit}, run, deny, removeResolver)
}
func uninstallLinuxUnits(ctx context.Context, cfg linuxUninstallConfig, run func(context.Context, string, ...string) ([]byte, error), deny func(context.Context) error, removeResolver func(context.Context, Config) error) (result UninstallResult, err error) {
	units, state, trust := cfg.UnitDir, cfg.StateDir, cfg.TrustDir

	if err = requirePrivilege(); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("device access removal incomplete; address protection and identity retained; retry pb daemon device-guard uninstall: %w", err)
		}
	}()
	service := filepath.Join(units, cfg.ServiceName)
	denyUnit := filepath.Join(units, cfg.DenyName)
	for _, path := range []string{service, denyUnit} {
		if err = validateGuardUnit(path); err != nil {
			return result, err
		}
	}
	_, serviceErr := os.Stat(service)
	_, denyErr := os.Stat(denyUnit)
	if os.IsNotExist(serviceErr) && os.IsNotExist(denyErr) {
		return result, nil
	}
	if err = protectedDirectory(state, 0700); err != nil {
		return result, err
	}
	command := func(name string, args ...string) error {
		out, e := run(ctx, name, args...)
		if e != nil {
			return fmt.Errorf("%s: %w: %s", name, e, out)
		}
		return nil
	}
	if serviceErr == nil {
		if err = command("systemctl", "disable", "--now", cfg.ServiceName); err != nil {
			return result, err
		}
	}
	lock, err := lockState(filepath.Join(state, "lock"))
	if err != nil {
		return result, err
	}
	defer lock.Close()
	// Never flush/delete the owned table. Replace live permissions with default deny.
	if err = deny(ctx); err != nil {
		return result, err
	}
	if err = writeGuardUnit(denyUnit, cfg.DenyUnit); err != nil {
		return result, err
	}
	if err = command("systemctl", "daemon-reload"); err != nil {
		return result, err
	}
	if err = command("systemctl", "enable", cfg.DenyName); err != nil {
		return result, err
	}
	result.Retained = []string{"boot address-deny service and protected executable", "address/name reservations and private CA identity"}
	roots, err := uninstallRoots(state)
	if err != nil {
		return result, err
	}
	if err = removeResolver(ctx, Config{StateDir: state, ConfigureResolver: true}); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "device listeners and owned resolver configuration")
	for _, root := range roots {
		digest := sha256.Sum256([]byte(root.suffix))
		path := filepath.Join(trust, fmt.Sprintf("paperboat-deviceguard-%x.crt", digest[:12]))
		info, e := os.Lstat(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return result, e
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return result, errors.New("unsafe owned trust certificate")
		}
		actual, e := os.ReadFile(path)
		if e != nil {
			return result, e
		}
		if !bytes.Equal(actual, root.pem) {
			return result, errors.New("trust certificate ownership conflict; foreign material preserved")
		}
		if e = os.Remove(path); e != nil {
			return result, e
		}
	}
	// Always retry rebuilding the trust database, including after source removal.
	if len(roots) > 0 {
		if err = command("update-ca-certificates"); err != nil {
			return result, err
		}
	}
	result.Removed = append(result.Removed, "owned private HTTPS system trust")
	if serviceErr == nil {
		if err = os.Remove(service); err != nil {
			return result, err
		}
	}
	if err = command("systemctl", "daemon-reload"); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "device access service")
	return result, nil
}
