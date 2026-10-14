//go:build darwin

package machineguard

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const darwinGuardLabel = "io.paperboat.machineguard"

// darwinAccountMarker is the RealName install writes into every account it
// creates; purge removes only accounts that carry it.
const darwinAccountMarker = "Paperboat protected listener"

// Purge completely removes the machine guard, including the deny-only launch
// job, address reservations and service accounts that Uninstall deliberately
// retains. It is used only when no Paperboat installation remains.
func Purge(ctx context.Context) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	return purgeDarwin(ctx, darwinPurge{
		Paths: darwinPurgePaths{
			LaunchPath:       darwinLaunchPath,
			HelperPath:       darwinHelperPath,
			StateDir:         DefaultStateDir,
			SocketDir:        filepath.Dir(DefaultSocket),
			Anchor:           darwinAnchor,
			LegacyLaunchPath: "/Library/LaunchDaemons/io.paperboat.deviceguard.plist",
			LegacyHelperPath: "/Library/PrivilegedHelperTools/io.paperboat.deviceguard",
			LegacyAnchor:     "com.apple/000.paperboat-deviceguard",
			LegacyStateDir:   "/Library/Application Support/Paperboat/deviceguard",
			LegacySocketDir:  "/Library/Application Support/Paperboat/deviceguard-control",
		},
		Run: runDarwinTrustRemoval,
		LocalNames: func(ctx context.Context) error {
			return cleanupHistoricalLocalNames(ctx, Config{StateDir: DefaultStateDir})
		},
	})
}

type darwinPurgePaths struct {
	LaunchPath, HelperPath                           string
	StateDir, SocketDir                              string
	Anchor                                           string
	LegacyLaunchPath, LegacyHelperPath, LegacyAnchor string
	LegacyStateDir, LegacySocketDir                  string
}

// darwinPurge carries every external effect so the purge order can be tested
// without launchd, pf, dscl or the System keychain.
type darwinPurge struct {
	Paths      darwinPurgePaths
	Run        func(context.Context, string, ...string) ([]byte, error)
	LocalNames func(context.Context) error
}

func purgeDarwin(ctx context.Context, p darwinPurge) (resultErr error) {
	if ctx == nil {
		return errors.New("machine guard purge context is missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	paths := p.Paths
	var result error
	if info, err := os.Lstat(paths.StateDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
			return errors.New("machine guard state directory is unsafe; preserved")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Installation takes this same lock. Create its protected namespace even
	// when no guard state remains, so orphan cleanup cannot race installation.
	lifecycle, err := lockGuardLifecycle(paths.StateDir)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lifecycle.Close()) }()
	legacyExists, err := legacyDarwinStateExists(paths.LegacyStateDir)
	if err != nil {
		return err
	}
	if legacyExists {
		legacyLifecycle, err := lockGuardLifecycle(paths.LegacyStateDir)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, legacyLifecycle.Close()) }()
	}

	// Launch files are removed only when they are Paperboat's own, following the
	// ownership rule install and uninstall enforce.
	if err := darwinCheckInstallOwnership(darwinInstallConfig{HelperPath: paths.HelperPath, LaunchPath: paths.LaunchPath}); err != nil {
		return fmt.Errorf("machine guard launch service preserved: %w", err)
	}
	if paths.LegacyLaunchPath != "" {
		if err := darwinCheckInstallOwnership(darwinInstallConfig{LaunchPath: paths.LegacyLaunchPath, HelperPath: paths.LegacyHelperPath, LaunchMarker: "<!-- Managed by Paperboat device guard -->"}); err != nil {
			return fmt.Errorf("legacy device guard preserved: %w", err)
		}
	}
	// Full purge stops the owned guard directly. Standalone Uninstall retains
	// denial and therefore copies/restarts a helper; that transition is neither
	// needed here nor possible after the account executable has been removed.
	if err := stopDarwinPurgeJob(ctx, p.Run, "system/"+darwinGuardLabel); err != nil {
		return err
	}
	if paths.LegacyLaunchPath != "" {
		if err := stopDarwinPurgeJob(ctx, p.Run, "system/io.paperboat.deviceguard"); err != nil {
			return err
		}
	}
	lock, err := lockStoppedGuard(ctx, paths.StateDir)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	if legacyExists {
		legacyLock, err := lockStoppedGuard(ctx, paths.LegacyStateDir)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, legacyLock.Close()) }()
	}

	result = errors.Join(result, removeOwnedPath(paths.LaunchPath), removeOwnedPath(paths.HelperPath))
	if paths.LegacyLaunchPath != "" {
		result = errors.Join(result, removeOwnedPath(paths.LegacyLaunchPath), removeOwnedPath(paths.LegacyHelperPath))
	}
	if paths.LegacyAnchor != "" {
		if _, err := p.Run(ctx, "/sbin/pfctl", "-a", paths.LegacyAnchor, "-F", "all"); err != nil {
			result = errors.Join(result, fmt.Errorf("flush legacy device guard PF anchor: %w", err))
		}
	}

	// Trust removal needs the retained root records, so the state directory is
	// kept whenever it fails.
	// Owned loopback aliases and the PF enable token are recorded in state.
	retirePlatform(Config{StateDir: paths.StateDir})
	result = errors.Join(result, darwinRetiredPlatformState(paths.StateDir))
	roots, err := uninstallRoots(paths.StateDir)
	if err != nil {
		result = errors.Join(result, fmt.Errorf("read machine guard trust state: %w", err))
	} else if len(roots) > 0 {
		result = errors.Join(result, cleanupDarwinTrustEntriesWithRunner(ctx, Config{StateDir: paths.StateDir}, roots, roots, true, p.Run))
	}
	result = errors.Join(result, removeBrowserDomainSelection(paths.StateDir))
	if legacyExists {
		result = errors.Join(result, retireLegacyDarwinState(ctx, paths.LegacyStateDir, p.Run))
	}

	// Full purge also retires orphaned owned roots whose protected manifests
	// were already removed by an interrupted historical cleanup. Namespace
	// retirement still uses its exact retained root records above.
	result = errors.Join(result, purgeDarwinOrphanTrust(ctx, p.Run))
	result = errors.Join(result, p.LocalNames(ctx))
	// Flushing the owned child anchor removes every retained deny rule.
	if _, err := p.Run(ctx, "/sbin/pfctl", "-a", paths.Anchor, "-F", "all"); err != nil {
		result = errors.Join(result, fmt.Errorf("flush machine guard PF anchor: %w", err))
	}

	for _, account := range []string{darwinBrokerAccount, "_paperboat_deviceguard"} {
		result = errors.Join(result, purgeDarwinAccount(ctx, p.Run, account))
	}
	result = errors.Join(result, removeOwnedPath(paths.SocketDir))
	result = errors.Join(result, removeEmptyLegacyDarwinControl(paths.LegacySocketDir))
	if result == nil {
		result = errors.Join(result, removeOwnedPath(paths.StateDir))
	} else {
		result = errors.Join(result, fmt.Errorf("machine guard state %s retained; fix the errors above and retry", paths.StateDir))
	}
	return result
}

func stopDarwinPurgeJob(ctx context.Context, run darwinTrustRunner, domain string) error {
	if _, err := run(ctx, "/bin/launchctl", "print", domain); err != nil {
		// launchctl print's job-not-found exit means this exact job is absent.
		// Permission, transport and other failures never authorize deletion.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 113 {
			return nil
		}
		return err
	}
	_, err := run(ctx, "/bin/launchctl", "bootout", domain)
	return err
}

// purgeDarwinAccount deletes a service account only when it carries the marker
// install writes. An account without the marker is foreign and is preserved.
func purgeDarwinAccount(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error), account string) error {
	path := "/Users/" + account
	realName, err := run(ctx, "/usr/bin/dscl", ".", "-read", path, "RealName")
	if err != nil {
		// dscl exits non-zero for an absent record; the account is then nothing to remove.
		return nil
	}
	if !strings.Contains(string(realName), darwinAccountMarker) {
		return fmt.Errorf("account %s is not owned by Paperboat; preserved", account)
	}
	if _, err := run(ctx, "/usr/bin/dscl", ".", "-delete", path); err != nil {
		return fmt.Errorf("delete machine guard account %s: %w", account, err)
	}
	return nil
}

// darwinRetiredPlatformState reports owned loopback aliases or a PF enable
// reference that retirePlatform could not release. retirePlatform does not return
// errors, so its leftovers are the only evidence of failure.
func darwinRetiredPlatformState(stateDir string) error {
	if _, err := os.Stat(filepath.Join(stateDir, "pf-enable-token")); err == nil {
		return errors.New("machine guard PF enable reference was not released")
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "aliases.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var aliases map[string]bool
	if err := json.Unmarshal(data, &aliases); err != nil {
		return fmt.Errorf("read owned loopback alias journal: %w", err)
	}
	if len(aliases) != 0 {
		return errors.New("machine guard loopback aliases were not removed")
	}
	return nil
}

func removeOwnedPath(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func purgeDarwinOrphanTrust(ctx context.Context, run darwinTrustRunner) error {
	fingerprints, err := darwinOwnedKeychainRoots(ctx, run)
	if err != nil {
		return fmt.Errorf("inspect orphaned machine guard trust: %w", err)
	}
	for _, fingerprint := range fingerprints {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := run(ctx, "/usr/bin/security", "delete-certificate", "-t", "-Z", fingerprint, darwinSystemKeychain); err != nil {
			return fmt.Errorf("remove orphaned machine guard trust: %w", err)
		}
	}
	if len(fingerprints) == 0 {
		return nil
	}
	remaining, err := darwinOwnedKeychainRoots(ctx, run)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("owned machine guard root certificates remain in the System keychain")
	}
	return nil
}

func darwinOwnedKeychainRoots(ctx context.Context, run darwinTrustRunner) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output, err := run(ctx, "/usr/bin/security", "find-certificate", "-a", "-p", "-c", "Paperboat Local Root CA", darwinSystemKeychain)
	if err != nil {
		return nil, err
	}
	if len(output) > 4<<20 {
		return nil, errors.New("machine guard root certificate inventory exceeds its bound")
	}
	found := map[string]bool{}
	for len(bytes.TrimSpace(output)) > 0 {
		block, rest := pem.Decode(output)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Bytes) > 32<<10 {
			return nil, errors.New("invalid System keychain certificate inventory")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, errors.New("invalid System keychain certificate")
		}
		if _, err := parseLocalTrustRoot(pem.EncodeToMemory(block)); err == nil {
			digest := sha1.Sum(block.Bytes)
			found[strings.ToUpper(hex.EncodeToString(digest[:]))] = true
			if len(found) > 128 {
				return nil, errors.New("machine guard root certificate inventory exceeds its bound")
			}
		}
		output = rest
	}
	fingerprints := make([]string, 0, len(found))
	for fingerprint := range found {
		fingerprints = append(fingerprints, fingerprint)
	}
	sort.Strings(fingerprints)
	return fingerprints, nil
}
