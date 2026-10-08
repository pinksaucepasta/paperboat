//go:build darwin

package machineguard

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
)

// ServeDenyOnly retains the cached-address boundary without exposing machine
// listeners, DNS, or a control socket. Its launch job survives uninstall/reboot.
func ServeDenyOnly(ctx context.Context) error {
	state, err := loadLoopbackState(DefaultStateDir)
	if err != nil {
		return err
	}
	state, err = mergedLoopbackState(state, machineloopback.DefaultCIDR)
	if err != nil {
		return err
	}
	if err = writeLoopbackState(DefaultStateDir, state); err != nil {
		return err
	}
	return serveDenyOnly(ctx, Config{StateDir: DefaultStateDir, LoopbackCIDR: machineloopback.DefaultCIDR, ProtectedLoopbackCIDRs: state.Protected})
}
func serveDenyOnly(ctx context.Context, cfg Config) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	if err := protectedDirectory(cfg.StateDir, 0700); err != nil {
		return err
	}
	lock, err := lockState(filepath.Join(cfg.StateDir, "lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = applyProtection(ctx, cfg, nil); err != nil {
		return err
	}
	if err = darwinWrite(filepath.Join(cfg.StateDir, "deny-ready"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(cfg.StateDir, "deny-ready"))
	<-ctx.Done()
	return nil // Do not retire the safety rules when launchd replaces this process.
}

func Uninstall(ctx context.Context) (result UninstallResult, err error) {
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
	state, err := loadLoopbackState(DefaultStateDir)
	if err != nil {
		return result, err
	}
	state, err = mergedLoopbackState(state, machineloopback.DefaultCIDR)
	if err != nil {
		return result, err
	}
	if err = writeLoopbackState(DefaultStateDir, state); err != nil {
		return result, err
	}
	return uninstallDarwin(ctx, darwinInstallConfig{HelperPath: darwinHelperPath, LaunchPath: darwinLaunchPath, Label: "io.paperboat.machineguard", Socket: DefaultSocket, Account: darwinBrokerAccount, LaunchArguments: []string{"daemon", "machine-guard", "run"}}, Config{StateDir: DefaultStateDir, LoopbackCIDR: machineloopback.DefaultCIDR, ProtectedLoopbackCIDRs: state.Protected}, []string{"daemon", "machine-guard", "deny-only"})
}
func uninstallDarwin(ctx context.Context, cfg darwinInstallConfig, guardCfg Config, denyArguments []string) (result UninstallResult, err error) {
	if err = requirePrivilege(); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("machine access removal incomplete; safety state retained; retry sudo pb daemon machine-guard uninstall: %w", err)
		}
	}()
	if err = darwinCheckInstallOwnership(cfg); err != nil {
		return result, err
	}
	if _, err = os.Stat(cfg.LaunchPath); os.IsNotExist(err) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	if err = protectedDirectory(guardCfg.StateDir, 0700); err != nil {
		return result, err
	}
	data, err := os.ReadFile(cfg.LaunchPath)
	if err != nil {
		return result, err
	}
	runArgs := uninstallDarwinArguments(cfg.LaunchArguments)
	denyArgs := uninstallDarwinArguments(denyArguments)
	if !strings.Contains(string(data), runArgs) && !strings.Contains(string(data), denyArgs) {
		return result, errors.New("owned launch job has unexpected arguments; preserved")
	}
	// The retained job must execute this version, including its deny-only command.
	executable, e := os.Executable()
	if e != nil {
		return result, e
	}
	if err = copyDarwinRecoveryExecutable(executable, cfg.HelperPath); err != nil {
		return result, err
	}
	// Persist the safe boot mode before stopping the running product service.
	data = []byte(strings.Replace(string(data), runArgs, denyArgs, 1))
	if err = darwinWrite(cfg.LaunchPath, data, 0644); err != nil {
		return result, err
	}
	domain := "system/" + cfg.Label
	if exec.CommandContext(ctx, "/bin/launchctl", "print", domain).Run() == nil {
		if _, err = darwinRun(ctx, "", "/bin/launchctl", "bootout", domain); err != nil {
			return result, err
		}
	}
	lock, err := lockStoppedGuard(ctx, guardCfg.StateDir)
	if err != nil {
		return result, err
	}
	locked := true
	defer func() {
		if locked {
			lock.Close()
		}
	}()
	if err = applyProtection(ctx, guardCfg, nil); err != nil {
		return result, err
	}
	result.Retained = []string{"boot deny-only launch job and protected executable", "address/name reservations", "protected listener service account"}
	roots, err := uninstallRoots(guardCfg.StateDir)
	if err != nil {
		return result, err
	}
	if err = cleanupHistoricalLocalNames(ctx, guardCfg); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "machine listeners and owned local name configuration")
	if err = cleanupDarwinOwnedTrust(ctx, guardCfg, roots); err != nil {
		return result, err
	}
	if err = removeBrowserDomainSelection(guardCfg.StateDir); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "owned private HTTPS system trust and CA state", "machine access launch mode")
	if err = lock.Close(); err != nil {
		return result, err
	}
	locked = false
	if err = os.Remove(filepath.Join(guardCfg.StateDir, "deny-ready")); err != nil && !os.IsNotExist(err) {
		return result, err
	}
	if _, err = darwinRun(ctx, "", "/bin/launchctl", "bootstrap", "system", cfg.LaunchPath); err != nil {
		return result, err
	}
	if err = waitDarwinDenyReady(ctx, domain, guardCfg.StateDir); err != nil {
		return result, err
	}
	return result, nil
}

func cleanupHistoricalTrust(ctx context.Context, cfg Config) error {
	roots, err := uninstallRoots(cfg.StateDir)
	if err != nil {
		return err
	}
	if err := cleanupDarwinOwnedTrust(ctx, cfg, roots); err != nil {
		return err
	}
	return removeBrowserDomainSelection(cfg.StateDir)
}

func cleanupDarwinOwnedTrust(ctx context.Context, cfg Config, roots []ownedRoot) error {
	return cleanupDarwinTrustEntries(ctx, cfg, roots, roots, true)
}

func runDarwinTrustRemoval(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "find-certificate" {
		return exec.CommandContext(ctx, name, args...).Output()
	}
	return darwinRun(ctx, "", name, args...)
}
func cleanupDarwinTrustEntries(ctx context.Context, cfg Config, knownRoots, removeRoots []ownedRoot, removeState bool) error {
	return cleanupDarwinTrustEntriesWithRunner(ctx, cfg, knownRoots, removeRoots, removeState, runDarwinTrustRemoval)
}
func cleanupDarwinTrustEntriesWithRunner(ctx context.Context, cfg Config, knownRoots, removeRoots []ownedRoot, removeState bool, run darwinTrustRunner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	trustDirectory := filepath.Join(cfg.StateDir, "trusted-ca")
	info, err := os.Lstat(trustDirectory)
	if os.IsNotExist(err) {
		if removeState {
			return removeOwnedRootState(removeRoots)
		}
		return nil
	} else if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("unsafe machine guard trust directory; preserved")
	}
	if err = protectedDirectory(trustDirectory, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(trustDirectory)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, root := range knownRoots {
		digest := sha256.Sum256(root.certificate.Raw)
		stem := hex.EncodeToString(digest[:]) + ".pem"
		allowed[stem] = true
		allowed[stem+".installed"] = true
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("unexpected machine guard trust manifest %q; preserved", entry.Name())
		}
	}
	for _, root := range knownRoots {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest := sha256.Sum256(root.certificate.Raw)
		stem := hex.EncodeToString(digest[:]) + ".pem"
		pemPath := filepath.Join(trustDirectory, stem)
		receiptPath := pemPath + ".installed"
		manifestPEM, pemErr := readOwnedTrustFile(pemPath)
		receipt, receiptErr := readOwnedTrustFile(receiptPath)
		if pemErr != nil && !os.IsNotExist(pemErr) {
			return pemErr
		}
		if receiptErr != nil && !os.IsNotExist(receiptErr) {
			return receiptErr
		}
		if manifestPEM != nil {
			block, rest := pem.Decode(manifestPEM)
			if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 || !bytes.Equal(block.Bytes, root.certificate.Raw) || !bytes.Equal(manifestPEM, root.pem) {
				return errors.New("machine guard trust manifest does not match retained root; preserved")
			}
		}
		if receipt != nil && string(receipt) != "installed\n" {
			return errors.New("machine guard trust receipt is invalid; preserved")
		}
	}
	for _, root := range removeRoots {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest := sha256.Sum256(root.certificate.Raw)
		stem := hex.EncodeToString(digest[:]) + ".pem"
		pemPath := filepath.Join(trustDirectory, stem)
		receiptPath := pemPath + ".installed"
		output, err := run(ctx, "/usr/bin/security", "find-certificate", "-a", "-Z", darwinSystemKeychain)
		if err != nil {
			return err
		}
		sha1Digest := sha1.Sum(root.certificate.Raw)
		fingerprint := strings.ToUpper(hex.EncodeToString(sha1Digest[:]))
		if darwinHasCertificateFingerprint(string(output), fingerprint) {
			if _, err = run(ctx, "/usr/bin/security", "delete-certificate", "-t", "-Z", fingerprint, darwinSystemKeychain); err != nil {
				return err
			}
		}
		for _, path := range []string{receiptPath, pemPath} {
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if removeState && len(removeRoots) > 0 {
		if err = removeOwnedRootState(removeRoots); err != nil {
			return err
		}
	}
	remaining, err := os.ReadDir(trustDirectory)
	if err == nil && len(remaining) == 0 {
		if err = os.Remove(trustDirectory); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readOwnedTrustFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("unsafe machine guard trust manifest; preserved")
	}
	return os.ReadFile(path)
}

func darwinHasCertificateFingerprint(output, fingerprint string) bool {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(strings.ToLower(line), "sha-1 hash:") {
			continue
		}
		value := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		value = strings.TrimPrefix(strings.ToUpper(value), "0X")
		if value == fingerprint {
			return true
		}
	}
	return false
}

func copyDarwinRecoveryExecutable(source, target string) (err error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(target), ".paperboat-recovery-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if _, err = io.Copy(out, in); err == nil {
		err = out.Chmod(0755)
	}
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		return err
	}
	return os.Rename(out.Name(), target)
}

func uninstallDarwinArguments(arguments []string) string {
	var out strings.Builder
	for _, argument := range arguments {
		out.WriteString("<string>")
		_ = xml.EscapeText(&out, []byte(argument))
		out.WriteString("</string>")
	}
	return out.String()
}

func waitDarwinDenyReady(ctx context.Context, domain, state string) error {
	ready, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		marker, err := os.ReadFile(filepath.Join(state, "deny-ready"))
		if err == nil {
			pid, parseErr := strconv.Atoi(string(marker))
			if parseErr == nil && pid > 0 {
				output, lookupErr := exec.CommandContext(ready, "/bin/launchctl", "print", domain).Output()
				if lookupErr == nil && strings.Contains(string(output), "pid = "+strconv.Itoa(pid)+"\n") {
					lock, lockErr := lockState(filepath.Join(state, "lock"))
					if errors.Is(lockErr, errGuardRunning) {
						return nil
					}
					if lockErr != nil {
						return lockErr
					}
					lock.Close()
				}
			}
		}
		select {
		case <-ready.Done():
			return fmt.Errorf("retained address protection job is not ready; retry uninstall: %w", ready.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
