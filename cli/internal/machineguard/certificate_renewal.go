//go:build linux || darwin || windows

package machineguard

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"io"
	"os"
	"path/filepath"
	"time"
)

const localCARenewalWindow = 30 * 24 * time.Hour

// PrepareLocalCARenewal is an installer-only transition. The caller must stop
// the owned guard first; the guard's state lock excludes a live signer. The
// public certificate journal survives interruption without losing the exact
// trust fingerprint to retire. Runtime never silently replaces trusted keys.
func PrepareLocalCARenewal(ctx context.Context, cfg Config) error {
	if err := requireInstallerPrivilege(); err != nil {
		return err
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if err := protectedDirectory(cfg.StateDir, 0700); err != nil {
		return err
	}
	lock, err := lockStoppedGuard(ctx, cfg.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	domains, err := approvedBrowserDomains(cfg.StateDir)
	if err != nil {
		return err
	}
	for domain := range domains {
		directory := filepath.Join(cfg.StateDir, "certificates", domain)
		if _, err := os.Lstat(directory); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := protectedDirectory(directory, 0700); err != nil {
			return err
		}
		journalName := "local-ca-renewal.pem"
		if domain != splitdns.BrowserSuffix {
			digest := sha256.Sum256([]byte(domain))
			journalName = "local-ca-renewal-" + hex.EncodeToString(digest[:16]) + ".pem"
		}
		if err := renewLocalCA(ctx, directory, filepath.Join(cfg.StateDir, journalName), time.Now(), validateOwnedRootStateFile, func(root []byte) error { return removeLocalTrust(ctx, cfg, root) }); err != nil {
			return err
		}
	}
	return nil
}
func readRenewalFile(path, name string, validate func(string, string, os.FileInfo) error) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("unsafe local CA renewal state; preserved")
	}
	if err := validate(path, name, info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("renewal state changed while opening; preserved")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<10 {
		return nil, errors.New("oversized local CA renewal state; preserved")
	}
	return data, nil
}

// Dependencies are narrow OS boundaries, allowing interruption recovery to be
// tested without altering the workspace machine's privileged trust database.
func renewLocalCA(ctx context.Context, directory, journal string, now time.Time, validate func(string, string, os.FileInfo) error, removeTrust func([]byte) error) error {
	old, err := readRenewalFile(journal, "local-ca-renewal.pem", validate)
	pending := err == nil
	if !pending && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rootPath := filepath.Join(directory, "rootCA.pem")
	current, currentErr := readRenewalFile(rootPath, "rootCA.pem", validate)
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return currentErr
	}
	if !pending {
		if errors.Is(currentErr, os.ErrNotExist) {
			return nil
		}
		root, err := parseLocalTrustRoot(current)
		if err != nil {
			return err
		}
		if root.NotAfter.After(now.Add(localCARenewalWindow)) {
			return nil
		}
		old = current
	}
	root, err := parseLocalTrustRoot(old)
	if err != nil {
		return err
	}
	if currentErr == nil && !bytes.Equal(old, current) {
		return errors.New("local CA changed during renewal; preserved")
	}
	expected := map[string][]byte{}
	if currentErr == nil {
		expected["rootCA.pem"] = current
	}
	// Validate every remaining artifact before removing trust or deleting state.
	for _, name := range []string{"rootCA-key.pem", "rootCA.crl"} {
		data, err := readRenewalFile(filepath.Join(directory, name), name, validate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		expected[name] = data
		if name == "rootCA-key.pem" {
			block, _ := pem.Decode(data)
			if block == nil {
				return errors.New("invalid renewal signing key; preserved")
			}
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			public, ok := root.PublicKey.(*rsa.PublicKey)
			if err != nil || !ok || !key.PublicKey.Equal(public) {
				return errors.New("renewal key differs from owned root; preserved")
			}
		} else {
			list, err := x509.ParseRevocationList(data)
			if err != nil || list.CheckSignatureFrom(root) != nil {
				return errors.New("renewal CRL differs from owned root; preserved")
			}
		}
	}
	if !pending {
		if err := writeRenewalJournal(journal, old); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := removeTrust(old); err != nil {
		return err
	}
	for _, name := range []string{"rootCA-key.pem", "rootCA.crl", "rootCA.pem"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(directory, name)
		if current, err := readRenewalFile(path, name, validate); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		} else if !bytes.Equal(current, expected[name]) {
			return errors.New("local CA artifact changed during renewal; preserved")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if err := os.Remove(journal); err != nil {
		return err
	}
	return nil
}
