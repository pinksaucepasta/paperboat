//go:build linux || darwin || windows

package machineguard

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

var errGuardRunning = errors.New("machine guard already running")

type ownedRoot struct {
	suffix      string
	directory   string
	pem         []byte
	certificate *x509.Certificate
}

// Only roots retained in protected guard state are candidates for trust removal.
// Expired roots are accepted here so failed renewal can always be recovered.
func uninstallRoots(state string) ([]ownedRoot, error) {
	directory := filepath.Join(state, "certificates")
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("unsafe machine guard certificate state directory; preserved")
	}
	if err = protectedDirectory(directory, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var roots []ownedRoot
	for _, entry := range entries {
		suffix := entry.Name()
		if !validOwnedTrustSuffix(suffix) {
			return nil, fmt.Errorf("unexpected certificate state entry %q", entry.Name())
		}
		path := filepath.Join(directory, suffix)
		if err = protectedDirectory(path, 0700); err != nil {
			return nil, err
		}
		path = filepath.Join(path, "rootCA.pem")
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("unsafe root certificate state")
		}
		if err = validateOwnedRootStateFile(path, "rootCA.pem", info); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" || strings.TrimSpace(string(rest)) != "" {
			return nil, errors.New("invalid retained root certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		if !cert.IsCA || cert.CheckSignatureFrom(cert) != nil || !cert.PermittedDNSDomainsCritical || len(cert.PermittedDNSDomains) != 1 || cert.PermittedDNSDomains[0] != "."+suffix {
			return nil, errors.New("retained certificate is not an owned constrained root")
		}
		if cert.Subject.CommonName != "Paperboat Local Root CA" || len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "Paperboat Local Development CA" {
			return nil, errors.New("retained certificate is not a Paperboat-owned root")
		}
		roots = append(roots, ownedRoot{suffix: suffix, directory: filepath.Dir(path), pem: data, certificate: cert})
	}
	return roots, nil
}

func validOwnedTrustSuffix(suffix string) bool {
	if domain, err := splitdns.NormalizeBrowserDomain(suffix); err == nil && domain == suffix {
		return true
	}
	if len(suffix) < 2 || len(suffix) > 16 {
		return false
	}
	for _, r := range suffix {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// removeOwnedRootState deletes only the previous local CA's known state files.
// It refuses unknown entries so user data in the protected directory survives.
func removeOwnedRootState(roots []ownedRoot) error {
	known := map[string]bool{"rootCA.pem": true, "rootCA-key.pem": true, "rootCA.crl": true, "lock": true}
	for _, root := range roots {
		if err := protectedDirectory(root.directory, 0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(root.directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !known[entry.Name()] {
				return fmt.Errorf("unexpected machine guard certificate state %q; preserved", entry.Name())
			}
			info, err := os.Lstat(filepath.Join(root.directory, entry.Name()))
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("unsafe machine guard certificate state; preserved")
			}
			if err = validateOwnedRootStateFile(filepath.Join(root.directory, entry.Name()), entry.Name(), info); err != nil {
				return err
			}
			if entry.Name() == "rootCA.pem" {
				data, err := os.ReadFile(filepath.Join(root.directory, entry.Name()))
				if err != nil {
					return err
				}
				if !bytes.Equal(data, root.pem) {
					return errors.New("machine guard certificate state changed during cleanup; preserved")
				}
			}
		}
	}
	parents := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		parents[filepath.Dir(root.directory)] = struct{}{}
		entries, err := os.ReadDir(root.directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err = os.Remove(filepath.Join(root.directory, entry.Name())); err != nil {
				return err
			}
		}
		if err = os.Remove(root.directory); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for directory := range parents {
		entries, err := os.ReadDir(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err = os.Remove(directory); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// Lifecycle operations and foreground trust enrollment share this lock. The
// runtime has a separate lock, allowing uninstall to stop it before taking that
// lock and enumerating a stable set of all owned roots.
func lockGuardLifecycle(state string) (interface{ Close() error }, error) {
	if err := protectedDirectory(state, 0700); err != nil {
		return nil, err
	}
	return lockState(filepath.Join(state, "lifecycle.lock"))
}

// launchctl bootout and Task Scheduler termination can return before the old
// process releases its state lock. Wait only for that bounded shutdown window.
func lockStoppedGuard(ctx context.Context, state string) (io.Closer, error) {
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		lock, err := lockState(filepath.Join(state, "lock"))
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, errGuardRunning) {
			return nil, err
		}
		select {
		case <-stop.Done():
			return nil, fmt.Errorf("machine guard has not stopped; retry removal: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
