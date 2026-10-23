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

// browserDomains is the root-owned record of the local access domains. Default
// is the deployment's domain, recorded when the guard is installed; an owner
// listed in Owners chose another one. There is no built-in domain.
type browserDomains struct {
	Schema  string            `json:"schema"`
	Default string            `json:"default,omitempty"`
	Owners  map[string]string `json:"owners"`
}

// ErrBrowserDomainUnset means the guard was installed without a local access
// domain, so browser access and machine names cannot be served yet.
var ErrBrowserDomainUnset = errors.New("local access domain is not set up; run pb install again")

// validateBrowserDomainsFile is the protected-state check of the domain record;
// unprivileged tests replace it because they cannot create root-owned files.
var validateBrowserDomainsFile = validateOwnedRootStateFile

func loadBrowserDomains(state string) (browserDomains, error) {
	result := browserDomains{Schema: browserDomainSchema, Owners: map[string]string{}}
	if state == "" {
		return result, nil
	} // In-memory guard test instances have no persistent state.
	data, err := readRenewalFile(filepath.Join(state, "browser-domains.json"), "browser-domains.json", validateBrowserDomainsFile)
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
	if result.Default != "" {
		if normalized, e := splitdns.NormalizeBrowserDomain(result.Default); e != nil || normalized != result.Default {
			return result, errors.New("invalid protected browser domain selection; preserved")
		}
	}
	for owner, domain := range result.Owners {
		normalized, e := splitdns.NormalizeBrowserDomain(domain)
		if !validBrowserDomainOwner(owner) || e != nil || normalized != domain || domain == result.Default {
			return result, errors.New("invalid protected browser domain selection; preserved")
		}
	}
	return result, nil
}

// selected is the domain owner's browser access uses: its own choice, else the deployment's.
func (d browserDomains) selected(owner string) (string, error) {
	if domain := d.Owners[owner]; domain != "" {
		return domain, nil
	}
	if d.Default == "" {
		return "", ErrBrowserDomainUnset
	}
	return d.Default, nil
}

// approved is every domain whose local CA this guard may hold.
func (d browserDomains) approved() map[string]bool {
	approved := map[string]bool{}
	if d.Default != "" {
		approved[d.Default] = true
	}
	for _, domain := range d.Owners {
		approved[domain] = true
	}
	return approved
}

// renewalJournal names the protected renewal journal of domain's CA.
func (d browserDomains) renewalJournal(domain string) string {
	if domain == d.Default {
		return "local-ca-renewal.pem"
	}
	digest := sha256.Sum256([]byte(domain))
	return "local-ca-renewal-" + hex.EncodeToString(digest[:16]) + ".pem"
}

func selectedBrowserDomain(state, owner string) (string, error) {
	domains, err := loadBrowserDomains(state)
	if err != nil {
		return "", err
	}
	return domains.selected(owner)
}

// recordInstallBrowserDomain records the deployment's local access domain when
// the installer knows it; an empty domain keeps what is recorded.
func recordInstallBrowserDomain(domain string) error {
	if domain == "" {
		return nil
	}
	if err := protectedDirectory(DefaultStateDir, 0700); err != nil {
		return err
	}
	return recordDefaultBrowserDomain(DefaultStateDir, domain)
}

// recordDefaultBrowserDomain stores the deployment's local access domain. An
// empty domain keeps the recorded one. Callers hold the guard lifecycle lock.
func recordDefaultBrowserDomain(state, domain string) error {
	if domain == "" {
		return nil
	}
	domain, err := splitdns.NormalizeBrowserDomain(domain)
	if err != nil {
		return err
	}
	domains, err := loadBrowserDomains(state)
	if err != nil {
		return err
	}
	if domains.Default == domain {
		return nil
	}
	domains.Default = domain
	for owner, selected := range domains.Owners {
		if selected == domain {
			delete(domains.Owners, owner)
		}
	}
	return writeBrowserDomains(state, domains)
}

func writeBrowserDomains(state string, domains browserDomains) error {
	data, err := json.Marshal(domains)
	if err != nil {
		return err
	}
	return writeRenewalJournal(filepath.Join(state, "browser-domains.json"), data)
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
	undo, err := Install(ctx, executable, "")
	if err != nil {
		return nil, err
	}
	undo.Discard()
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
	previous, _ := domains.selected(owner)
	if previous == domain {
		return nil, nil
	}
	if _, ok := domains.Owners[owner]; !ok && len(domains.Owners) >= 512 {
		return nil, errors.New("browser domain owner capacity reached")
	}
	approved := domains.approved()
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
	if domain == domains.Default {
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
	if previous == "" || previous == domains.Default {
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
	domains, err := loadBrowserDomains(state)
	if err != nil {
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
		if name != domains.renewalJournal(domain) {
			return errors.New("unowned local CA renewal journal; preserved")
		}
		if err := os.Remove(filepath.Join(state, name)); err != nil {
			return err
		}
	}
	return nil
}
