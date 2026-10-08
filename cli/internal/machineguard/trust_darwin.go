//go:build darwin

package machineguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const darwinSystemKeychain = "/Library/Keychains/System.keychain"

type darwinTrustRunner func(context.Context, string, ...string) ([]byte, error)

func installLocalTrust(ctx context.Context, cfg Config, certificatePEM []byte) error {
	return installDarwinLocalTrust(ctx, cfg, certificatePEM, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	})
}

func installDarwinLocalTrust(ctx context.Context, cfg Config, certificatePEM []byte, run darwinTrustRunner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	certificate, err := parseLocalTrustRoot(certificatePEM)
	if err != nil {
		return err
	}
	directory := filepath.Join(cfg.StateDir, "trusted-ca")
	if err = protectedDirectory(directory, 0700); err != nil {
		return err
	}
	digest := sha256.Sum256(certificate.Raw)
	pemPath := filepath.Join(directory, hex.EncodeToString(digest[:])+".pem")
	if err = writeOwnedTrustState(pemPath, certificatePEM, 0644); err != nil {
		return err
	}
	return ensureDarwinSystemTrust(ctx, pemPath, run)
}

func ensureDarwinSystemTrust(ctx context.Context, pemPath string, run darwinTrustRunner) error {
	if err := verifyDarwinSystemTrust(ctx, pemPath, run); err == nil {
		return recordDarwinTrustOwnership(pemPath)
	}
	if _, err := run(ctx, "/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", darwinSystemKeychain, pemPath); err != nil {
		return fmt.Errorf("install Paperboat HTTPS root in the macOS System keychain: %w", err)
	}
	if err := verifyDarwinSystemTrust(ctx, pemPath, run); err != nil {
		return fmt.Errorf("macOS did not accept the Paperboat root for SSL trust; check system keychain or managed certificate policy: %w", err)
	}
	return recordDarwinTrustOwnership(pemPath)
}

func verifyDarwinSystemTrust(ctx context.Context, pemPath string, run darwinTrustRunner) error {
	_, err := run(ctx, "/usr/bin/security", "verify-cert", "-p", "ssl", "-c", pemPath, "-k", darwinSystemKeychain)
	return err
}

func recordDarwinTrustOwnership(pemPath string) error {
	if err := writeOwnedTrustState(pemPath+".installed", []byte("installed\n"), 0600); err != nil {
		return fmt.Errorf("record macOS root trust ownership: %w", err)
	}
	return nil
}

func removeLocalTrust(ctx context.Context, cfg Config, certificatePEM []byte) error {
	return removeDarwinLocalTrust(ctx, cfg, certificatePEM, runDarwinTrustRemoval)
}
func removeDarwinLocalTrust(ctx context.Context, cfg Config, certificatePEM []byte, run darwinTrustRunner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	certificate, err := parseLocalTrustRoot(certificatePEM)
	if err != nil {
		return err
	}
	requested := ownedRoot{suffix: localTrustDomain(certificate), pem: certificatePEM, certificate: certificate}
	known, err := uninstallRoots(cfg.StateDir)
	if err != nil {
		return err
	}
	found := false
	for _, root := range known {
		if bytes.Equal(root.certificate.Raw, certificate.Raw) {
			found = true
			break
		}
	}
	// Interrupted renewal can retain the exact root only in its protected public
	// journal. It must still be recognized without retiring another namespace.
	if !found {
		known = append(known, requested)
	}
	return cleanupDarwinTrustEntriesWithRunner(ctx, cfg, known, []ownedRoot{requested}, false, run)
}

func cleanupHistoricalTrustExcept(ctx context.Context, cfg Config, activePEM []byte) error {
	retired, err := historicalRootsExcept(cfg.StateDir, activePEM)
	if err != nil || len(retired) == 0 {
		return err
	}
	all, err := uninstallRoots(cfg.StateDir)
	if err != nil {
		return err
	}
	return cleanupDarwinTrustEntries(ctx, cfg, all, retired, true)
}

func InstallUserTrust(context.Context, []byte) error { return nil }
func RemoveUserTrust(context.Context, []byte) error  { return nil }
func CleanupUserTrust(context.Context) error         { return nil }

func writeOwnedTrustState(path string, data []byte, mode os.FileMode) (resultErr error) {
	if existing, err := readOwnedTrustFile(path); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return errors.New("Paperboat trust state conflicts with existing file; preserved")
	} else if !os.IsNotExist(err) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".trust-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err = temporary.Chmod(mode); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	err = errors.Join(err, temporary.Close())
	if err != nil {
		return err
	}
	if existing, readErr := readOwnedTrustFile(path); readErr == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return errors.New("Paperboat trust state changed during update; preserved")
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if err = os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	return nil
}
