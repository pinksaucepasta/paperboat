//go:build linux

package machineguard

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxUserTrustReceiptBytes = 1 << 20
const userTrustRefreshInterval = 30 * time.Second

type nssCommand func(context.Context, ...string) ([]byte, error)

type linuxNSSDatabase struct {
	Path   string
	Create bool
}

type linuxUserTrustReceipt struct {
	Version     int                             `json:"version"`
	Certificate []byte                          `json:"certificate"`
	Databases   []linuxUserTrustDatabaseReceipt `json:"databases"`
}

type linuxUserTrustDatabaseReceipt struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

var linuxUserTrustMu sync.Mutex
var linuxUserTrustCache struct {
	sync.Mutex
	entries map[string]linuxUserTrustCacheEntry
}

type linuxUserTrustCacheEntry struct {
	paths string
	until time.Time
}

func installLinuxUserTrust(ctx context.Context, certificatePEM []byte, run nssCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	certificate, err := parseLocalTrustRoot(certificatePEM)
	if err != nil {
		return err
	}
	uid := uint32(os.Geteuid())
	if uid == 0 {
		return errors.New("local browser trust must be installed by the signed-in user session")
	}
	home, err := currentUserHome()
	if err != nil {
		return err
	}
	databases, err := discoverLinuxNSSDatabases(home)
	if err != nil {
		return err
	}
	if len(databases) > 0 {
		if _, err = exec.LookPath("certutil"); err != nil {
			return errors.New("Linux browser certificate support requires NSS certutil; install the Paperboat NSS trust prerequisite and retry")
		}
	}
	paths := linuxTrustDatabaseSet(databases)
	cacheKey := fmt.Sprintf("%d:%s:%s", uid, home, localTrustFingerprint(certificate))
	linuxUserTrustCache.Lock()
	if cached, ok := linuxUserTrustCache.entries[cacheKey]; ok && cached.paths == paths && time.Now().Before(cached.until) {
		linuxUserTrustCache.Unlock()
		return nil
	}
	linuxUserTrustCache.Unlock()

	linuxUserTrustMu.Lock()
	defer linuxUserTrustMu.Unlock()
	linuxUserTrustCache.Lock()
	if cached, ok := linuxUserTrustCache.entries[cacheKey]; ok && cached.paths == paths && time.Now().Before(cached.until) {
		linuxUserTrustCache.Unlock()
		return nil
	}
	linuxUserTrustCache.Unlock()
	if err = cleanupLinuxUserTrustExcept(ctx, certificate, run); err != nil {
		return err
	}
	if len(databases) == 0 {
		cacheLinuxUserTrust(cacheKey, paths)
		return nil
	}
	if err = installLinuxUserTrustDatabases(ctx, home, uid, certificate, certificatePEM, databases, run); err != nil {
		return err
	}
	cacheLinuxUserTrust(cacheKey, paths)
	return nil
}

func cacheLinuxUserTrust(cacheKey, paths string) {
	linuxUserTrustCache.Lock()
	if linuxUserTrustCache.entries == nil {
		linuxUserTrustCache.entries = make(map[string]linuxUserTrustCacheEntry)
	}
	linuxUserTrustCache.entries[cacheKey] = linuxUserTrustCacheEntry{paths: paths, until: time.Now().Add(userTrustRefreshInterval)}
	linuxUserTrustCache.Unlock()
}

func installLinuxUserTrustDatabases(ctx context.Context, home string, uid uint32, certificate *x509.Certificate, certificatePEM []byte, databases []linuxNSSDatabase, run nssCommand) error {
	stateDirectory, err := linuxUserTrustStateDirectory(home, uid, true)
	if err != nil {
		return err
	}
	fingerprint := localTrustFingerprint(certificate)
	receiptPath := filepath.Join(stateDirectory, fingerprint+".json")
	receipt, err := readLinuxUserTrustReceipt(receiptPath, uid, certificate)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		receipt = linuxUserTrustReceipt{Version: 1, Certificate: append([]byte(nil), certificatePEM...)}
	}
	if err := verifyLinuxUserTrustDirectory(stateDirectory, home, uid); err != nil {
		return err
	}
	certificatePath := filepath.Join(stateDirectory, fingerprint+".pending.pem")
	if err = writeLinuxUserTrustState(certificatePath, certificatePEM, uid, 0600); err != nil {
		return err
	}
	defer os.Remove(certificatePath)
	for _, database := range databases {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err = ensureLinuxNSSDatabase(ctx, database, home, uid, run); err != nil {
			return fmt.Errorf("prepare browser certificate database %s: %w", filepath.Base(database.Path), err)
		}
		relative, err := filepath.Rel(home, database.Path)
		if err != nil || !userTrustRelativePath(relative) {
			return errors.New("browser certificate database is outside the signed-in user's home")
		}
		entryIndex := findLinuxUserTrustDatabase(receipt.Databases, relative)
		if entryIndex >= 0 && receipt.Databases[entryIndex].State != "pending" && receipt.Databases[entryIndex].State != "installed" {
			return errors.New("invalid Paperboat browser trust receipt; preserved")
		}
		present, err := nssLocalRootTrusted(ctx, database.Path, certificate, run)
		if err != nil {
			return fmt.Errorf("inspect browser certificate database %s: %w", filepath.Base(database.Path), err)
		}
		if present {
			if entryIndex < 0 {
				// Matching material predates our ownership receipt. Leave it alone.
				continue
			}
			receipt.Databases[entryIndex].State = "installed"
			if err = writeLinuxUserTrustReceipt(receiptPath, uid, receipt); err != nil {
				return err
			}
			continue
		}
		if entryIndex < 0 {
			receipt.Databases = append(receipt.Databases, linuxUserTrustDatabaseReceipt{Path: relative, State: "pending"})
			entryIndex = len(receipt.Databases) - 1
		} else {
			receipt.Databases[entryIndex].State = "pending"
		}
		if err = writeLinuxUserTrustReceipt(receiptPath, uid, receipt); err != nil {
			return err
		}
		if _, err = run(ctx, "-A", "-d", "sql:"+database.Path, "-t", "C,,", "-n", localTrustNickname(certificate), "-i", certificatePath); err != nil {
			return fmt.Errorf("install Paperboat root in browser certificate database %s; close the browser and retry: %w", filepath.Base(database.Path), err)
		}
		receipt.Databases[entryIndex].State = "installed"
		if err = writeLinuxUserTrustReceipt(receiptPath, uid, receipt); err != nil {
			return err
		}
	}
	if len(receipt.Databases) == 0 {
		if err = os.Remove(receiptPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func removeLinuxUserTrust(ctx context.Context, certificatePEM []byte, run nssCommand) error {
	linuxUserTrustMu.Lock()
	defer linuxUserTrustMu.Unlock()
	certificate, err := parseLocalTrustRoot(certificatePEM)
	if err != nil {
		return err
	}
	err = removeLinuxUserTrustReceipt(ctx, certificate, run)
	if err == nil {
		clearLinuxUserTrustCache()
	}
	return err
}

func cleanupLinuxUserTrust(ctx context.Context, run nssCommand) error {
	linuxUserTrustMu.Lock()
	defer linuxUserTrustMu.Unlock()
	err := cleanupLinuxUserTrustExcept(ctx, nil, run)
	if err == nil {
		clearLinuxUserTrustCache()
	}
	return err
}

func cleanupLinuxUserTrustExcept(ctx context.Context, active *x509.Certificate, run nssCommand) error {
	uid := uint32(os.Geteuid())
	if uid == 0 {
		return errors.New("local browser trust cleanup must run in the signed-in user session")
	}
	home, err := currentUserHome()
	if err != nil {
		return err
	}
	stateDirectory, err := linuxUserTrustStateDirectory(home, uid, false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(stateDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return fmt.Errorf("unexpected Paperboat browser trust state %q; preserved", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".pending.pem") {
			path := filepath.Join(stateDirectory, entry.Name())
			if err = verifyLocalUserTrustFile(path, uid, false); err != nil {
				return err
			}
			certificatePEM, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			certificate, parseErr := parseLocalTrustRoot(certificatePEM)
			if parseErr != nil || entry.Name() != localTrustFingerprint(certificate)+".pending.pem" {
				return errors.New("invalid Paperboat pending browser trust certificate; preserved")
			}
			if active != nil && bytes.Equal(active.Raw, certificate.Raw) {
				continue
			}
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("unexpected Paperboat browser trust state %q; preserved", entry.Name())
		}
		certificatePEM, readErr := receiptCertificate(filepath.Join(stateDirectory, entry.Name()), uid)
		if readErr != nil {
			return readErr
		}
		certificate, parseErr := parseLocalTrustRoot(certificatePEM)
		if parseErr != nil {
			return parseErr
		}
		if entry.Name() != localTrustFingerprint(certificate)+".json" {
			return errors.New("Paperboat browser trust receipt fingerprint mismatch; preserved")
		}
		if active != nil && bytes.Equal(active.Raw, certificate.Raw) {
			continue
		}
		if err = removeLinuxUserTrustReceipt(ctx, certificate, run); err != nil {
			return err
		}
	}
	if entries, err = os.ReadDir(stateDirectory); err != nil {
		return err
	} else if len(entries) == 0 {
		if err = os.Remove(stateDirectory); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func removeLinuxUserTrustReceipt(ctx context.Context, certificate *x509.Certificate, run nssCommand) error {
	uid := uint32(os.Geteuid())
	if uid == 0 {
		return errors.New("local browser trust cleanup must run in the signed-in user session")
	}
	home, err := currentUserHome()
	if err != nil {
		return err
	}
	stateDirectory, err := linuxUserTrustStateDirectory(home, uid, false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	receiptPath := filepath.Join(stateDirectory, localTrustFingerprint(certificate)+".json")
	receipt, err := readLinuxUserTrustReceipt(receiptPath, uid, certificate)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, database := range receipt.Databases {
		if err = ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(home, filepath.Clean(database.Path))
		if !userTrustRelativePath(database.Path) {
			return errors.New("unsafe database path in Paperboat browser trust receipt; preserved")
		}
		if err = verifyLinuxUserNSSDatabase(path, home, uid, false); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		present, checkErr := nssLocalRootPresent(ctx, path, certificate, run)
		if checkErr != nil {
			return fmt.Errorf("inspect browser certificate database %s: %w", filepath.Base(path), checkErr)
		}
		if present {
			if _, err = run(ctx, "-D", "-d", "sql:"+path, "-n", localTrustNickname(certificate)); err != nil {
				return fmt.Errorf("remove Paperboat root from browser certificate database %s: %w", filepath.Base(path), err)
			}
		}
	}
	return os.Remove(receiptPath)
}

func discoverLinuxNSSDatabases(home string) ([]linuxNSSDatabase, error) {
	uid := uint32(os.Geteuid())
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	canonicalHome, err = filepath.Abs(canonicalHome)
	if err != nil {
		return nil, err
	}
	var databases []linuxNSSDatabase
	seen := map[string]bool{}
	add := func(path string, create bool) error {
		canonical, exists, err := canonicalUserDirectory(path, canonicalHome, uid, create)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !exists && !create || seen[canonical] {
			return nil
		}
		if !create && !isNSSSQLDatabase(canonical) {
			return nil
		}
		seen[canonical] = true
		databases = append(databases, linuxNSSDatabase{Path: canonical, Create: create})
		return nil
	}

	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" || !filepath.IsAbs(dataHome) || !pathWithin(canonicalHome, filepath.Clean(dataHome)) {
		dataHome = filepath.Join(canonicalHome, ".local", "share")
	}
	chromeInstalled := linuxChromiumInstalled(canonicalHome)
	if err = add(filepath.Join(dataHome, "pki", "nssdb"), chromeInstalled); err != nil {
		return nil, err
	}
	if err = add(filepath.Join(canonicalHome, ".pki", "nssdb"), false); err != nil {
		return nil, err
	}

	for _, app := range []string{"chromium", "chrome"} {
		for _, root := range []string{
			filepath.Join(canonicalHome, "snap", app, "common"),
			filepath.Join(canonicalHome, "snap", app, "current"),
		} {
			if _, err = os.Stat(root); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return nil, err
			}
			create := filepath.Base(root) == "common"
			for _, relative := range []string{filepath.Join(".local", "share", "pki", "nssdb"), filepath.Join(".pki", "nssdb")} {
				if err = add(filepath.Join(root, relative), create && relative == filepath.Join(".local", "share", "pki", "nssdb")); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, app := range []string{"org.chromium.Chromium", "com.google.Chrome"} {
		root := filepath.Join(canonicalHome, ".var", "app", app)
		if _, err = os.Stat(root); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, relative := range []string{
			filepath.Join("data", "pki", "nssdb"),
			filepath.Join("data", ".local", "share", "pki", "nssdb"),
			filepath.Join("data", ".pki", "nssdb"),
			filepath.Join(".local", "share", "pki", "nssdb"),
			filepath.Join(".pki", "nssdb"),
		} {
			if err = add(filepath.Join(root, relative), relative == filepath.Join("data", "pki", "nssdb")); err != nil {
				return nil, err
			}
		}
	}

	for _, profileRoot := range linuxFirefoxProfileRoots(canonicalHome) {
		if err = addLinuxFirefoxProfiles(&databases, seen, profileRoot, canonicalHome, uid); err != nil {
			return nil, err
		}
	}
	return databases, nil
}

func linuxFirefoxProfileRoots(home string) []string {
	return []string{
		filepath.Join(home, ".mozilla", "firefox"),
		filepath.Join(home, "snap", "firefox", "common", ".mozilla", "firefox"),
		filepath.Join(home, "snap", "firefox", "current", ".mozilla", "firefox"),
		filepath.Join(home, ".var", "app", "org.mozilla.firefox", ".mozilla", "firefox"),
		filepath.Join(home, ".var", "app", "org.mozilla.firefox", "data", ".mozilla", "firefox"),
	}
}

func addLinuxFirefoxProfiles(databases *[]linuxNSSDatabase, seen map[string]bool, root, home string, uid uint32) error {
	canonicalRoot, exists, err := canonicalUserDirectory(root, home, uid, false)
	if os.IsNotExist(err) || !exists {
		return nil
	}
	if err != nil {
		return err
	}
	data, err := readLinuxFirefoxProfileIndex(filepath.Join(canonicalRoot, "profiles.ini"), uid, 1<<20)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("Firefox profile index exceeds size limit")
	}
	for _, profile := range parseFirefoxProfiles(data) {
		if profile.IsRelative && !filepath.IsAbs(profile.Path) {
			profile.Path = filepath.Join(canonicalRoot, profile.Path)
		}
		canonical, profileExists, err := canonicalUserDirectory(profile.Path, home, uid, false)
		if os.IsNotExist(err) || !profileExists {
			continue
		}
		if err != nil {
			return err
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		*databases = append(*databases, linuxNSSDatabase{Path: canonical, Create: true})
	}
	return nil
}

type firefoxProfile struct {
	Path       string
	IsRelative bool
}

func parseFirefoxProfiles(data []byte) []firefoxProfile {
	var profiles []firefoxProfile
	section := false
	path := ""
	relative := true
	flush := func() {
		if section && path != "" {
			profiles = append(profiles, firefoxProfile{Path: path, IsRelative: relative})
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			section = strings.HasPrefix(line, "[Profile")
			path, relative = "", true
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !section {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Path":
			path = strings.TrimSpace(value)
		case "IsRelative":
			relative = strings.TrimSpace(value) == "1"
		}
	}
	flush()
	return profiles
}

func linuxChromiumInstalled(home string) bool {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	for _, path := range []string{
		"/snap/bin/chromium", "/snap/bin/google-chrome",
		filepath.Join(home, "snap", "chromium", "common"),
		filepath.Join(home, ".var", "app", "org.chromium.Chromium"),
		filepath.Join(home, ".var", "app", "com.google.Chrome"),
	} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func currentUserHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("signed-in user home is not absolute")
	}
	return filepath.EvalSymlinks(home)
}

func canonicalUserDirectory(path, boundary string, uid uint32, allowMissing bool) (string, bool, error) {
	if !filepath.IsAbs(path) {
		return "", false, errors.New("browser certificate path is not absolute")
	}
	path = filepath.Clean(path)
	boundary, err := filepath.EvalSymlinks(boundary)
	if err != nil {
		return "", false, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if os.IsNotExist(err) && allowMissing {
		parent := filepath.Dir(path)
		for {
			resolvedParent, parentErr := filepath.EvalSymlinks(parent)
			if parentErr == nil {
				relative, relErr := filepath.Rel(resolvedParent, path)
				if relErr != nil {
					return "", false, relErr
				}
				resolved = filepath.Join(resolvedParent, relative)
				break
			}
			if !os.IsNotExist(parentErr) || parent == filepath.Dir(parent) {
				return "", false, parentErr
			}
			parent = filepath.Dir(parent)
		}
	} else if os.IsNotExist(err) {
		return "", false, err
	} else if err != nil {
		return "", false, err
	}
	if !pathWithin(boundary, resolved) {
		return "", false, errors.New("browser certificate path resolves outside the signed-in user's home")
	}
	info, err := os.Stat(resolved)
	if os.IsNotExist(err) && allowMissing {
		return resolved, false, nil
	}
	if err != nil {
		return "", false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return "", false, errors.New("unsafe browser certificate directory ownership or permissions")
	}
	return resolved, true, nil
}

func pathWithin(boundary, path string) bool {
	relative, err := filepath.Rel(boundary, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isNSSSQLDatabase(path string) bool {
	for _, name := range []string{"cert9.db", "key4.db", "pkcs11.txt"} {
		info, err := os.Lstat(filepath.Join(path, name))
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func ensureLinuxNSSDatabase(ctx context.Context, database linuxNSSDatabase, home string, uid uint32, run nssCommand) error {
	path, exists, err := canonicalUserDirectory(database.Path, home, uid, database.Create)
	if err != nil {
		return err
	}
	if !exists && !database.Create {
		return fs.ErrNotExist
	}
	if !exists {
		if err = os.MkdirAll(path, 0700); err != nil {
			return err
		}
		path, exists, err = canonicalUserDirectory(path, home, uid, false)
		if err != nil || !exists {
			return errors.Join(err, errors.New("cannot create browser certificate database"))
		}
	}
	if err = verifyLinuxUserNSSDatabase(path, home, uid, false); err != nil && !os.IsNotExist(err) {
		return err
	}
	if isNSSSQLDatabase(path) {
		return nil
	}
	if !database.Create {
		return fs.ErrNotExist
	}
	if _, err = run(ctx, "-N", "-d", "sql:"+path, "--empty-password"); err != nil {
		return fmt.Errorf("initialize SQL certificate database: %w", err)
	}
	if !isNSSSQLDatabase(path) {
		return errors.New("certutil did not create an SQL certificate database")
	}
	return nil
}

func nssLocalRootPresent(ctx context.Context, database string, certificate *x509.Certificate, run nssCommand) (bool, error) {
	return nssLocalRoot(ctx, database, certificate, run, false)
}
func nssLocalRootTrusted(ctx context.Context, database string, certificate *x509.Certificate, run nssCommand) (bool, error) {
	return nssLocalRoot(ctx, database, certificate, run, true)
}
func nssLocalRoot(ctx context.Context, database string, certificate *x509.Certificate, run nssCommand, requireSSLTrust bool) (bool, error) {
	nickname := localTrustNickname(certificate)
	listing, err := run(ctx, "-L", "-d", "sql:"+database)
	if err != nil {
		return false, fmt.Errorf("list NSS certificates: %w", err)
	}
	for _, line := range strings.Split(string(listing), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != nickname {
			continue
		}
		encoded, err := run(ctx, "-L", "-d", "sql:"+database, "-n", nickname, "-a")
		if err != nil {
			return false, fmt.Errorf("read existing NSS certificate: %w", err)
		}
		block, rest := pem.Decode(encoded)
		if block == nil || block.Type != "CERTIFICATE" || strings.TrimSpace(string(rest)) != "" || !bytes.Equal(block.Bytes, certificate.Raw) {
			return false, errors.New("Paperboat NSS certificate nickname is occupied by foreign material; preserved")
		}
		trust := strings.Split(fields[len(fields)-1], ",")
		if requireSSLTrust && (len(trust) != 3 || !strings.Contains(trust[0], "C")) {
			return false, errors.New("Paperboat NSS root exists without SSL trust; existing trust settings preserved")
		}
		return true, nil
	}
	return false, nil
}

func linuxTrustDatabaseSet(databases []linuxNSSDatabase) string {
	paths := make([]string, 0, len(databases))
	for _, database := range databases {
		paths = append(paths, database.Path)
	}
	sort.Strings(paths)
	return strings.Join(paths, "\n")
}

func clearLinuxUserTrustCache() {
	linuxUserTrustCache.Lock()
	linuxUserTrustCache.entries = nil
	linuxUserTrustCache.Unlock()
}

func linuxUserTrustStateDirectory(home string, uid uint32, create bool) (string, error) {
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	if config == "" || !filepath.IsAbs(config) || !pathWithin(home, filepath.Clean(config)) {
		config = filepath.Join(home, ".config")
	}
	directory := filepath.Join(config, "paperboat", "machineguard", "trusted-ca")
	canonical, exists, err := canonicalUserDirectory(directory, home, uid, create)
	if err == nil && !exists && create {
		if err = os.MkdirAll(canonical, 0700); err == nil {
			canonical, exists, err = canonicalUserDirectory(canonical, home, uid, false)
		}
	}
	if err != nil || !exists {
		return canonical, err
	}
	if err = verifyLinuxUserTrustDirectory(canonical, home, uid); err != nil {
		return "", err
	}
	if err = os.Chmod(canonical, 0700); err != nil {
		return "", err
	}
	return canonical, nil
}

func readLinuxUserTrustReceipt(path string, uid uint32, certificate *x509.Certificate) (linuxUserTrustReceipt, error) {
	var receipt linuxUserTrustReceipt
	data, err := readLinuxUserTrustState(path, uid, maxUserTrustReceiptBytes)
	if err != nil {
		return receipt, err
	}
	if err = json.Unmarshal(data, &receipt); err != nil {
		return receipt, errors.New("invalid Paperboat browser trust receipt; preserved")
	}
	if receipt.Version != 1 || len(receipt.Databases) > 128 {
		return receipt, errors.New("unsupported Paperboat browser trust receipt; preserved")
	}
	root, err := parseLocalTrustRoot(receipt.Certificate)
	if err != nil || !bytes.Equal(root.Raw, certificate.Raw) {
		return receipt, errors.New("Paperboat browser trust receipt root mismatch; preserved")
	}
	for _, database := range receipt.Databases {
		if !userTrustRelativePath(database.Path) || database.State != "pending" && database.State != "installed" {
			return receipt, errors.New("invalid database entry in Paperboat browser trust receipt; preserved")
		}
	}
	return receipt, nil
}

func receiptCertificate(path string, uid uint32) ([]byte, error) {
	data, err := readLinuxUserTrustState(path, uid, maxUserTrustReceiptBytes)
	if err != nil {
		return nil, err
	}
	var receipt linuxUserTrustReceipt
	if err = json.Unmarshal(data, &receipt); err != nil || receipt.Version != 1 || len(receipt.Databases) > 128 {
		return nil, errors.New("invalid Paperboat browser trust receipt; preserved")
	}
	return receipt.Certificate, nil
}

func writeLinuxUserTrustReceipt(path string, uid uint32, receipt linuxUserTrustReceipt) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(data) > maxUserTrustReceiptBytes {
		return errors.New("Paperboat browser trust receipt exceeds size limit")
	}
	return writeLinuxUserTrustState(path, data, uid, 0600)
}

func readLinuxUserTrustState(path string, uid uint32, maxBytes int64) ([]byte, error) {
	if err := verifyLocalUserTrustFile(path, uid, false); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("Paperboat browser trust state exceeds size limit")
	}
	return data, nil
}

func readLinuxFirefoxProfileIndex(path string, uid uint32, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !safeLinuxFirefoxProfileIndex(info, uid) {
		return nil, errors.New("unsafe Firefox profile index; preserved")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, openedInfo) || !safeLinuxFirefoxProfileIndex(openedInfo, uid) {
		return nil, errors.New("unsafe Firefox profile index; preserved")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("Firefox profile index exceeds size limit")
	}
	return data, nil
}

func safeLinuxFirefoxProfileIndex(info os.FileInfo, uid uint32) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Mode().Perm()&0022 == 0
}

func writeLinuxUserTrustState(path string, data []byte, uid uint32, mode os.FileMode) (resultErr error) {
	if existing, err := readLinuxUserTrustState(path, uid, maxUserTrustReceiptBytes); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		if mode.Perm() == 0600 && strings.HasSuffix(path, ".json") {
			// Receipt updates are atomic and allowed only after ownership validation.
		} else {
			return errors.New("Paperboat browser trust state conflicts with existing file; preserved")
		}
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
	if err = os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	return nil
}

func verifyLocalUserTrustFile(path string, uid uint32, allowMissing bool) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && allowMissing {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe Paperboat browser trust state file; preserved")
	}
	return nil
}

func verifyLinuxUserNSSDatabase(path, home string, uid uint32, create bool) error {
	canonical, exists, err := canonicalUserDirectory(path, home, uid, create)
	if err != nil {
		return err
	}
	if !exists || canonical != filepath.Clean(path) {
		return errors.New("unsafe browser certificate database path")
	}
	for _, name := range []string{"cert9.db", "key4.db", "pkcs11.txt"} {
		entry := filepath.Join(canonical, name)
		info, err := os.Lstat(entry)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uid || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return errors.New("unsafe browser certificate database file")
		}
	}
	return nil
}

func userTrustRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	return clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func findLinuxUserTrustDatabase(databases []linuxUserTrustDatabaseReceipt, path string) int {
	for index := range databases {
		if databases[index].Path == path {
			return index
		}
	}
	return -1
}

func verifyLinuxUserTrustDirectory(path, home string, uid uint32) error {
	canonical, exists, err := canonicalUserDirectory(path, home, uid, false)
	if err != nil {
		return err
	}
	if !exists || canonical != filepath.Clean(path) {
		return errors.New("unsafe Paperboat browser trust directory")
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("Paperboat browser trust directory permissions are unsafe")
	}
	return nil
}
