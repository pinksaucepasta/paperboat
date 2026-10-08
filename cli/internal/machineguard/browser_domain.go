//go:build linux || darwin || windows

package machineguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

const browserDomainSchema = "paperboat.browser-domains/v1"

type browserDomains struct {
	Schema string            `json:"schema"`
	Owners map[string]string `json:"owners"`
}

func loadBrowserDomains(state string) (browserDomains, error) {
	result := browserDomains{Schema: browserDomainSchema, Owners: map[string]string{}}
	if state == "" {
		return result, nil
	} // In-memory guard test instances have no persistent state.
	data, err := readRenewalFile(filepath.Join(state, "browser-domains.json"), "browser-domains.json", validateOwnedRootStateFile)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return decodeBrowserDomains(data)
}
func decodeBrowserDomains(data []byte) (browserDomains, error) {
	result := browserDomains{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, errors.New("invalid protected browser domain selection; reinstall Paperboat")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, errors.New("invalid trailing protected browser domain selection")
	}
	if result.Schema != browserDomainSchema || result.Owners == nil || len(result.Owners) > 512 {
		return result, errors.New("invalid protected browser domain selection; preserved")
	}
	for owner, domain := range result.Owners {
		normalized, e := splitdns.NormalizeBrowserDomain(domain)
		if !validBrowserDomainOwner(owner) || e != nil || normalized != domain || domain == splitdns.BrowserSuffix {
			return result, errors.New("invalid protected browser domain selection; preserved")
		}
	}
	return result, nil
}
func selectedBrowserDomain(state, owner string) (string, error) {
	domains, err := loadBrowserDomains(state)
	if err != nil {
		return "", err
	}
	if domain := domains.Owners[owner]; domain != "" {
		return domain, nil
	}
	return splitdns.BrowserSuffix, nil
}
func approvedBrowserDomains(state string) (map[string]bool, error) {
	domains, err := loadBrowserDomains(state)
	if err != nil {
		return nil, err
	}
	approved := map[string]bool{splitdns.BrowserSuffix: true}
	for _, domain := range domains.Owners {
		approved[domain] = true
	}
	return approved, nil
}

// InstallBrowserDomain is an explicit elevated operation. User configuration
// alone cannot create a CA or expand the guard's certificate authority.
func InstallBrowserDomain(ctx context.Context, executable, owner, domain string) ([][]byte, error) {
	if err := requireInstallerPrivilege(); err != nil {
		return nil, err
	}
	if err := validateBrowserDomainCaller(owner); err != nil {
		return nil, err
	}
	domain, err := splitdns.NormalizeBrowserDomain(domain)
	if err != nil {
		return nil, err
	}
	// The normal installer repairs and restarts the owned helper before changing
	// its selection, and preserves its existing rollback guarantees.
	if err := Install(ctx, executable); err != nil {
		return nil, err
	}
	lifecycle, err := lockGuardLifecycle(DefaultStateDir)
	if err != nil {
		return nil, err
	}
	defer lifecycle.Close()
	return applyBrowserDomain(ctx, Config{StateDir: DefaultStateDir}, owner, domain)
}

func applyBrowserDomain(ctx context.Context, cfg Config, owner, domain string) ([][]byte, error) {
	return applyBrowserDomainTrust(ctx, cfg, owner, domain, installLocalTrust, removeLocalTrust)
}
func applyBrowserDomainTrust(ctx context.Context, cfg Config, owner, domain string, installTrust, removeTrust func(context.Context, Config, []byte) error) ([][]byte, error) {
	domains, err := loadBrowserDomains(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	previous := domains.Owners[owner]
	if previous == "" {
		previous = splitdns.BrowserSuffix
	}
	if previous == domain {
		return nil, nil
	}
	if _, ok := domains.Owners[owner]; !ok && len(domains.Owners) >= 512 {
		return nil, errors.New("browser domain owner capacity reached")
	}
	approved, err := approvedBrowserDomains(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	ca, err := prepareNamespaceCA(cfg.StateDir, domain, false)
	if err != nil {
		return nil, err
	}
	root := ca.CertPEM()
	cleanupNew := func() error {
		if approved[domain] {
			return nil
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := removeTrust(cleanupCtx, cfg, root); err != nil {
			return err
		}
		roots, err := uninstallRoots(cfg.StateDir)
		if err != nil {
			return err
		}
		for _, r := range roots {
			if r.suffix == domain {
				return removeOwnedRootState([]ownedRoot{r})
			}
		}
		return nil
	}
	if err := installTrust(ctx, cfg, root); err != nil {
		return nil, errors.Join(err, cleanupNew())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, cleanupNew())
	}
	if domain == splitdns.BrowserSuffix {
		delete(domains.Owners, owner)
	} else {
		domains.Owners[owner] = domain
	}
	data, err := json.Marshal(domains)
	if err == nil && len(data) > 64<<10 {
		err = errors.New("protected browser domain selection capacity reached")
	}
	if err == nil {
		err = writeRenewalJournal(filepath.Join(cfg.StateDir, "browser-domains.json"), data)
	}
	if err != nil {
		return nil, errors.Join(err, cleanupNew())
	}
	if previous == splitdns.BrowserSuffix {
		return nil, nil
	}
	for _, selected := range domains.Owners {
		if selected == previous {
			return nil, nil
		}
	}
	roots, err := uninstallRoots(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("browser domain changed; retiring previous trust: %w", err)
	}
	for _, r := range roots {
		if r.suffix != previous {
			continue
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = removeTrust(cleanupCtx, cfg, r.pem)
		cancel()
		if err == nil {
			err = removeOwnedRootState([]ownedRoot{r})
		}
		if err != nil {
			return nil, fmt.Errorf("browser domain changed; previous owned trust retained for installer cleanup: %w", err)
		}
		return [][]byte{r.pem}, nil
	}
	return nil, nil
}

// Full guard uninstall retires all system trust first, then removes only the
// bounded protected selection and validated public renewal journals.
func removeBrowserDomainSelection(state string) error {
	if _, err := loadBrowserDomains(state); err != nil {
		return err
	}
	path := filepath.Join(state, "browser-domains.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != "local-ca-renewal.pem" && !(strings.HasPrefix(name, "local-ca-renewal-") && strings.HasSuffix(name, ".pem")) {
			continue
		}
		data, err := readRenewalFile(filepath.Join(state, name), name, validateOwnedRootStateFile)
		if err != nil {
			return err
		}
		cert, err := parseLocalTrustRoot(data)
		if err != nil {
			return err
		}
		domain := localTrustDomain(cert)
		expected := "local-ca-renewal.pem"
		if domain != splitdns.BrowserSuffix {
			hash := sha256.Sum256([]byte(domain))
			expected = "local-ca-renewal-" + hex.EncodeToString(hash[:16]) + ".pem"
		}
		if name != expected {
			return errors.New("unowned local CA renewal journal; preserved")
		}
		if err := os.Remove(filepath.Join(state, name)); err != nil {
			return err
		}
	}
	return nil
}
