//go:build darwin

package deviceguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/deviceloopback"
)

// ServeDenyOnly retains the cached-address boundary without exposing device
// listeners, DNS, or a control socket. Its launch job survives uninstall/reboot.
func ServeDenyOnly(ctx context.Context) error {
	state, err := loadLoopbackState(DefaultStateDir)
	if err != nil {
		return err
	}
	dnsAddress, _ := deviceloopback.DNSAddress(state.Active)
	return serveDenyOnly(ctx, Config{StateDir: DefaultStateDir, LoopbackCIDR: state.Active, ProtectedLoopbackCIDRs: state.Protected, DNSAddress: net.JoinHostPort(dnsAddress.String(), defaultDNSPort)})
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
	dnsAddress, _ := deviceloopback.DNSAddress(state.Active)
	return uninstallDarwin(ctx, darwinInstallConfig{HelperPath: darwinHelperPath, LaunchPath: darwinLaunchPath, Label: "io.paperboat.deviceguard", Socket: DefaultSocket, Account: darwinBrokerAccount, LaunchArguments: []string{"daemon", "device-guard", "run"}}, Config{StateDir: DefaultStateDir, LoopbackCIDR: state.Active, ProtectedLoopbackCIDRs: state.Protected, DNSAddress: net.JoinHostPort(dnsAddress.String(), defaultDNSPort), ConfigureResolver: true}, []string{"daemon", "device-guard", "deny-only"})
}
func uninstallDarwin(ctx context.Context, cfg darwinInstallConfig, guardCfg Config, denyArguments []string) (result UninstallResult, err error) {
	if err = requirePrivilege(); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("device access removal incomplete; safety state retained; retry sudo pb daemon device-guard uninstall: %w", err)
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
	result.Retained = []string{"boot deny-only launch job and protected executable", "address/name reservations and private CA identity", "protected listener service account"}
	roots, err := uninstallRoots(guardCfg.StateDir)
	if err != nil {
		return result, err
	}
	if err = configureDomains(ctx, guardCfg, nil); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "device listeners and owned resolver configuration")
	for _, root := range roots {
		digest := sha256.Sum256(root.certificate.Raw)
		fingerprint := strings.ToUpper(hex.EncodeToString(digest[:]))
		output, e := exec.CommandContext(ctx, "/usr/bin/security", "find-certificate", "-a", "-Z", "/Library/Keychains/System.keychain").Output()
		if e != nil {
			return result, e
		}
		if strings.Contains(string(output), fingerprint) {
			if _, e = darwinRun(ctx, "", "/usr/bin/security", "delete-certificate", "-t", "-Z", fingerprint, "/Library/Keychains/System.keychain"); e != nil {
				return result, e
			}
		}
		receipt := filepath.Join(darwinTrustDirectory, strings.ToLower(fingerprint)+".pem.installed")
		if e = os.Remove(receipt); e != nil && !os.IsNotExist(e) {
			return result, e
		}
	}
	result.Removed = append(result.Removed, "owned private HTTPS system trust", "device access launch mode")
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
