//go:build darwin

package machineguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// These directories are the original deviceguard producer's namespaces. Their
// data uses the same authoritative guard formats; no compatibility reader is used.
func legacyDarwinStateExists(state string) (bool, error) {
	if state == "" {
		return false, nil
	}
	info, err := os.Lstat(state)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return false, errors.New("unsafe legacy guard state; preserved")
	}
	if err := protectedDirectory(state, 0700); err != nil {
		return false, err
	}
	return true, nil
}

func retireLegacyDarwinState(ctx context.Context, state string, run darwinTrustRunner) error {
	entries, err := os.ReadDir(state)
	if err != nil {
		return err
	}
	var residual []string
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(state, name)
		if name == "certificates" || name == "trusted-ca" {
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
				return errors.New("unsafe legacy guard trust directory; preserved")
			}
			if err := protectedDirectory(path, 0700); err != nil {
				return err
			}
			continue
		}
		if name == "browser-domains.json" || name == "local-ca-renewal.pem" || strings.HasPrefix(name, "local-ca-renewal-") && strings.HasSuffix(name, ".pem") {
			continue
		}
		switch name {
		case "aliases.json", "loopback-ranges.json", "pf-enable-token", "lock", "lifecycle.lock":
		default:
			return errors.New("unexpected legacy guard state; preserved")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1<<20 {
			return errors.New("unsafe legacy guard journal; preserved")
		}
		if (name == "lock" || name == "lifecycle.lock") && info.Size() != 0 {
			return errors.New("unexpected legacy guard lock data; preserved")
		}
		residual = append(residual, path)
	}
	ranges, err := loadLoopbackState(state)
	if err != nil {
		return err
	}
	cfg := Config{StateDir: state, LoopbackCIDR: ranges.Active, ProtectedLoopbackCIDRs: ranges.Protected}
	darwinPlatform.Lock()
	err = darwinLoadAliases(cfg)
	darwinPlatform.Unlock()
	if err != nil {
		return err
	}
	roots, err := uninstallRoots(state)
	if err != nil {
		return err
	}
	if err := cleanupDarwinTrustEntriesWithRunner(ctx, cfg, roots, roots, true, run); err != nil {
		return err
	}
	if err := removeBrowserDomainSelection(state); err != nil {
		return err
	}
	retirePlatform(cfg)
	if err := darwinRetiredPlatformState(state); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, path := range append(residual, filepath.Join(state, "aliases.json")) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Trust consumers validate and remove owned contents; empty containers may
	// remain when there were no retained roots. Unknown files keep the state.
	for _, name := range []string{"certificates", "trusted-ca"} {
		if err := os.Remove(filepath.Join(state, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Remove(state)
}

func removeEmptyLegacyDarwinControl(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("unsafe legacy guard control directory; preserved")
	}
	if err := protectedDirectory(path, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Remove(path)
}
