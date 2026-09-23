//go:build darwin

package deviceguard

import (
	"context"
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
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

const darwinHelperPath = "/Library/PrivilegedHelperTools/io.paperboat.deviceguard"
const darwinLaunchPath = "/Library/LaunchDaemons/io.paperboat.deviceguard.plist"
const darwinLaunchMarker = "<!-- Managed by Paperboat device guard -->"

func Install(ctx context.Context, executable string, configuredCIDR ...string) (resultErr error) {
	if err := requirePrivilege(); err != nil {
		return err
	}
	lifecycle, err := lockGuardLifecycle(DefaultStateDir)
	if err != nil {
		return err
	}
	defer lifecycle.Close()
	oldState, err := loadLoopbackState(DefaultStateDir)
	if err != nil {
		return err
	}
	cidr := oldState.Active
	if len(configuredCIDR) > 0 && strings.TrimSpace(configuredCIDR[0]) != "" {
		cidr = configuredCIDR[0]
	}
	cidr, fence, err := prepareLoopbackCIDRChange(ctx, cidr)
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.Close()
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, writeLoopbackState(DefaultStateDir, oldState))
		}
	}()
	if err := storeLoopbackCIDR(DefaultStateDir, cidr); err != nil {
		return err
	}

	return installDarwin(ctx, executable, darwinInstallConfig{
		HelperPath: darwinHelperPath, LaunchPath: darwinLaunchPath,
		Label: "io.paperboat.deviceguard", Socket: DefaultSocket, Account: darwinBrokerAccount,
		LaunchArguments: []string{"daemon", "device-guard", "run"}, InstallAccount: true,
	})
}

type darwinInstallConfig struct {
	HelperPath, LaunchPath, Label, Socket, Account string
	LaunchArguments                                []string
	InstallAccount                                 bool
}

func installDarwin(ctx context.Context, executable string, cfg darwinInstallConfig) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	if cfg.HelperPath == "" || cfg.LaunchPath == "" || cfg.Label == "" || cfg.Socket == "" || !filepath.IsAbs(cfg.HelperPath) || !filepath.IsAbs(cfg.LaunchPath) || !filepath.IsAbs(cfg.Socket) || strings.ContainsAny(cfg.Label, "<>&/\x00\r\n") {
		return errors.New("invalid Darwin device guard install configuration")
	}
	for _, directory := range []string{filepath.Dir(cfg.HelperPath), filepath.Dir(cfg.LaunchPath)} {
		if err := protectedDirectory(directory, 0755); err != nil {
			return err
		}
	}
	if err := darwinCheckInstallOwnership(cfg); err != nil {
		return err
	}
	if cfg.InstallAccount {
		if err := darwinInstallAccount(ctx, cfg.Account); err != nil {
			return err
		}
	}
	if old, err := os.ReadFile(cfg.LaunchPath); err == nil && !strings.Contains(string(old), darwinLaunchMarker) {
		return errors.New("existing device guard launch service is not owned by Paperboat")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.CreateTemp(filepath.Dir(cfg.HelperPath), ".paperboat-helper-")
	if err != nil {
		return err
	}
	temp := target.Name()
	defer os.Remove(temp)
	if _, err = io.Copy(target, source); err == nil {
		err = target.Chmod(0755)
	}
	if err == nil {
		err = target.Sync()
	}
	err = errors.Join(err, target.Close())
	if err != nil {
		return err
	}
	programArguments := append([]string{cfg.HelperPath}, cfg.LaunchArguments...)
	var argumentXML strings.Builder
	for _, argument := range programArguments {
		argumentXML.WriteString("<string>")
		if err := xml.EscapeText(&argumentXML, []byte(argument)); err != nil {
			return err
		}
		argumentXML.WriteString("</string>")
	}
	unit := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
%s
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array>%s</array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>ProcessType</key><string>Background</string><key>ThrottleInterval</key><integer>2</integer>
<key>Umask</key><integer>63</integer>
</dict></plist>
`, darwinLaunchMarker, cfg.Label, argumentXML.String())
	if err = darwinWrite(cfg.LaunchPath, []byte(unit), 0644); err != nil {
		return err
	}
	if _, err = darwinRun(ctx, "", "/usr/bin/plutil", "-lint", cfg.LaunchPath); err != nil {
		return err
	}
	if err = os.Rename(temp, cfg.HelperPath); err != nil {
		return err
	}
	// A prior installed copy can be running; only this exact service is replaced.
	domain := "system/" + cfg.Label
	if exec.CommandContext(ctx, "/bin/launchctl", "print", domain).Run() == nil {
		if _, err = darwinRun(ctx, "", "/bin/launchctl", "bootout", domain); err != nil {
			return err
		}
	}
	_, err = darwinRun(ctx, "", "/bin/launchctl", "bootstrap", "system", cfg.LaunchPath)
	if err != nil {
		return err
	}
	ready, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		client, connectErr := Connect(ready, cfg.Socket)
		if connectErr == nil {
			connectErr = client.ReplaceNames(ready, nil)
			client.Close()
			if connectErr == nil {
				return nil
			}
		}
		select {
		case <-ready.Done():
			return fmt.Errorf("device guard installed but not ready; inspect launchctl print %s and retry installation: %w", domain, ready.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func darwinCheckInstallOwnership(cfg darwinInstallConfig) error {
	ownedUnit := false
	for _, path := range []string{cfg.LaunchPath, cfg.HelperPath} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("existing guard installation path is not a protected root-owned file: %s", path)
		}
		if path == cfg.LaunchPath {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			ownedUnit = strings.Contains(string(data), darwinLaunchMarker)
			if !ownedUnit {
				return errors.New("existing guard launch service is not owned by Paperboat")
			}
		} else if !ownedUnit {
			return errors.New("existing guard executable has no Paperboat service ownership record")
		}
	}
	return nil
}

func darwinInstallAccount(ctx context.Context, account string) error {
	if account == "" || strings.ContainsAny(account, "/:\x00\r\n") {
		return errors.New("invalid device guard account")
	}
	path := "/Users/" + account
	listing, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-list", "/Users").Output()
	if err != nil {
		return err
	}
	exists := false
	for _, name := range strings.Fields(string(listing)) {
		if name == account {
			exists = true
		}
	}
	if exists {
		marker, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", path, "RealName").Output()
		if err != nil || !strings.Contains(string(marker), "Paperboat protected listener") {
			return errors.New("existing guard account is not owned by Paperboat")
		}
		// Read the complete owned record: a missing UniqueID after interruption is
		// different from an unreadable record or an existing invalid identity.
		record, readErr := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", path).Output()
		if readErr != nil {
			return readErr
		}
		foundUID := false
		for _, line := range strings.Split(string(record), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != "UniqueID:" {
				continue
			}
			foundUID = true
			if len(fields) != 2 {
				return errors.New("existing guard account has an invalid system UID")
			}
			uid, parseErr := strconv.Atoi(fields[1])
			if parseErr != nil || uid < 400 || uid > 499 {
				return errors.New("existing guard account has an invalid system UID")
			}
		}
		if foundUID {
			// Never reassign an existing identity. Complete its disabled login state.
			for _, fields := range [][]string{{"PrimaryGroupID", "20"}, {"UserShell", "/usr/bin/false"}, {"NFSHomeDirectory", "/var/empty"}, {"IsHidden", "1"}, {"Password", "*"}, {"AuthenticationAuthority", ";DisabledUser;"}} {
				if _, err = darwinRun(ctx, "", "/usr/bin/dscl", append([]string{".", "-create", path}, fields...)...); err != nil {
					return err
				}
			}
			return nil
		}
		// Our marker exists but no identity was assigned: finish the interrupted
		// creation using a currently unused UID, written only after disabling login.

	}
	output, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-list", "/Users", "UniqueID").Output()
	if err != nil {
		return err
	}
	used := map[int]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if id, e := strconv.Atoi(fields[1]); e == nil {
				used[id] = true
			}
		}
	}
	uid := 499
	for uid >= 400 && used[uid] {
		uid--
	}
	if uid < 400 {
		return errors.New("no unused system UID for the device guard")
	}
	// A non-login service identity owns only listener sockets. It has no account
	// credentials, writable application state, or interactive shell.
	for _, fields := range [][]string{{"RealName", "Paperboat protected listener"}, {"PrimaryGroupID", "20"}, {"UserShell", "/usr/bin/false"}, {"NFSHomeDirectory", "/var/empty"}, {"IsHidden", "1"}, {"Password", "*"}, {"AuthenticationAuthority", ";DisabledUser;"}, {"UniqueID", strconv.Itoa(uid)}} {
		args := append([]string{".", "-create", path}, fields...)
		if _, err = darwinRun(ctx, "", "/usr/bin/dscl", args...); err != nil {
			return err
		}
	}
	return nil
}

// InstallTrust runs in the administrator's foreground login session so macOS
// can request approval. It never changes the system authorization policy.
func InstallTrust(ctx context.Context, suffix string) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	lifecycle, lockErr := lockGuardLifecycle(DefaultStateDir)
	if lockErr != nil {
		return lockErr
	}
	defer lifecycle.Close()
	if err := requirePrivilege(); err != nil {
		return err
	}
	suffix, err := splitdns.ValidateSuffix(suffix)
	if err != nil {
		return err
	}
	directory := filepath.Join(DefaultStateDir, "certificates", suffix)
	if err = protectedDirectory(directory, 0700); err != nil {
		return err
	}
	lock, err := lockState(filepath.Join(directory, "lock"))
	if err != nil {
		return fmt.Errorf("private certificate enrollment is busy; retry: %w", err)
	}
	defer lock.Close()
	ca, err := splitdns.LoadOrCreateConstrainedCA(directory, suffix)
	if err != nil {
		return err
	}
	return installCATrust(ctx, "0", suffix, ca.CertPEM())
}

var darwinTrustDirectory = filepath.Join(DefaultStateDir, "trusted-ca")

func installCATrust(ctx context.Context, owner, suffix string, certificatePEM []byte) error {
	// Only public material reaches the system keychain; the constrained CA key
	// remains inside the root-owned guard state and signing enforces name ownership.
	block, rest := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("invalid device guard trust certificate")
	}
	digest := sha256.Sum256(block.Bytes)
	directory := darwinTrustDirectory
	if err := protectedDirectory(directory, 0700); err != nil {
		return err
	}
	path := filepath.Join(directory, hex.EncodeToString(digest[:])+".pem")
	if _, err := os.Stat(path + ".installed"); err == nil {
		output, lookupErr := exec.CommandContext(ctx, "/usr/bin/security", "find-certificate", "-a", "-Z", "/Library/Keychains/System.keychain").Output()
		if lookupErr == nil && strings.Contains(string(output), strings.ToUpper(hex.EncodeToString(digest[:]))) {
			// Presence alone does not prove trust: an administrator may have
			// revoked trust without removing the certificate from the keychain.
			if darwinVerifyCATrust(ctx, path) == nil {
				return nil
			}
		}
	}
	if err := darwinWrite(path, certificatePEM, 0600); err != nil {
		return err
	}
	_, err := darwinRun(ctx, "", "/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", "/Library/Keychains/System.keychain", path)
	if err != nil {
		return fmt.Errorf("macOS has not approved Paperboat private HTTPS trust; in an interactive Mac terminal run sudo pb daemon device-guard trust --suffix %s, approve the system prompt, then retry device access: %w", suffix, err)
	}
	return darwinWrite(path+".installed", []byte("installed\n"), 0600)
}

func darwinVerifyCATrust(ctx context.Context, path string) error {
	return exec.CommandContext(ctx, "/usr/bin/security", "verify-cert", "-c", path, "-p", "basic", "-l", "-L", "-k", "/Library/Keychains/System.keychain").Run()
}
