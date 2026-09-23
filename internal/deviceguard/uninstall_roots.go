//go:build linux || darwin || windows

package deviceguard

import (
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

var errGuardRunning = errors.New("device guard already running")

type ownedRoot struct {
	suffix      string
	pem         []byte
	certificate *x509.Certificate
}

// Only roots retained in protected guard state are candidates for trust removal.
// Expired roots are accepted here so failed renewal can always be recovered.
func uninstallRoots(state string) ([]ownedRoot, error) {
	directory := filepath.Join(state, "certificates")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = protectedDirectory(directory, 0700); err != nil {
		return nil, err
	}
	var roots []ownedRoot
	for _, entry := range entries {
		suffix, err := splitdns.ValidateSuffix(entry.Name())
		if err != nil {
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
		if !cert.IsCA || !cert.PermittedDNSDomainsCritical || len(cert.PermittedDNSDomains) != 1 || cert.PermittedDNSDomains[0] != "."+suffix {
			return nil, errors.New("retained certificate is not an owned constrained root")
		}
		roots = append(roots, ownedRoot{suffix, data, cert})
	}
	return roots, nil
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
			return nil, fmt.Errorf("device guard has not stopped; retry removal: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
