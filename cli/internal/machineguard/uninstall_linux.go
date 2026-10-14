//go:build linux

package machineguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
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
	}, RestoreDeny, cleanupHistoricalLocalNames)
}

type linuxUninstallConfig struct{ UnitDir, StateDir, TrustDir, ServiceName, DenyName, DenyUnit string }

func uninstallLinux(ctx context.Context, units, state, trust string, run func(context.Context, string, ...string) ([]byte, error), deny func(context.Context) error, cleanupNames func(context.Context, Config) error) (result UninstallResult, err error) {
	return uninstallLinuxUnits(ctx, linuxUninstallConfig{units, state, trust, "paperboat-machineguard.service", "paperboat-machineguard-deny.service", guardDenyUnit}, run, deny, cleanupNames)
}
func uninstallLinuxUnits(ctx context.Context, cfg linuxUninstallConfig, run func(context.Context, string, ...string) ([]byte, error), deny func(context.Context) error, cleanupNames func(context.Context, Config) error) (result UninstallResult, err error) {
	units, state, trust := cfg.UnitDir, cfg.StateDir, cfg.TrustDir

	if err = requirePrivilege(); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("machine access removal incomplete; address protection and identity retained; retry pb daemon machine-guard uninstall: %w", err)
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
		roots, rootsErr := uninstallRoots(state)
		if rootsErr != nil {
			return result, rootsErr
		}
		if cleanupNames != nil {
			if err = cleanupNames(ctx, Config{StateDir: state, LoopbackCIDR: machineloopback.DefaultCIDR}); err != nil {
				return result, err
			}
			result.Removed = append(result.Removed, "historical owned local name integration")
		}
		if err = removeLinuxOwnedTrust(ctx, roots, trust, run); err != nil {
			return result, err
		}
		if err = removeBrowserDomainSelection(state); err != nil {
			return result, err
		}
		if len(roots) != 0 {
			result.Removed = append(result.Removed, "historical owned certificate trust and CA state")
		}
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
	result.Retained = []string{"boot address-deny service and protected executable", "address/name reservations"}
	roots, err := uninstallRoots(state)
	if err != nil {
		return result, err
	}
	if err = cleanupNames(ctx, Config{StateDir: state, LoopbackCIDR: machineloopback.DefaultCIDR}); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "machine listeners and owned local name configuration")
	if err = removeLinuxOwnedTrust(ctx, roots, trust, run); err != nil {
		return result, err
	}
	if err = removeBrowserDomainSelection(state); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "owned private HTTPS system trust and CA state")
	if serviceErr == nil {
		if err = os.Remove(service); err != nil {
			return result, err
		}
	}
	if err = command("systemctl", "daemon-reload"); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "machine access service")
	return result, nil
}

func cleanupHistoricalTrust(ctx context.Context, cfg Config) error {
	roots, err := uninstallRoots(cfg.StateDir)
	if err != nil {
		return err
	}
	if err := removeLinuxOwnedTrust(ctx, roots, "/usr/local/share/ca-certificates", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}); err != nil {
		return err
	}
	return removeBrowserDomainSelection(cfg.StateDir)
}

func removeLinuxOwnedTrust(ctx context.Context, roots []ownedRoot, trust string, run func(context.Context, string, ...string) ([]byte, error)) error {
	return removeLinuxTrustFiles(ctx, roots, trust, true, run)
}

func removeLinuxTrustFiles(ctx context.Context, roots []ownedRoot, trust string, removeState bool, run func(context.Context, string, ...string) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, root := range roots {
		path := linuxOwnedTrustPath(trust, root.suffix)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return errors.New("unsafe owned trust certificate")
		}
		actual, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, root.pem) {
			return errors.New("trust certificate ownership conflict; foreign material preserved")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	if len(roots) == 0 {
		return nil
	}
	if output, err := run(ctx, "update-ca-certificates"); err != nil {
		return fmt.Errorf("update system certificate trust after removing owned roots: %w: %s", err, output)
	}
	if removeState {
		return removeOwnedRootState(roots)
	}
	return nil
}
