//go:build linux

package machineguard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const linuxSystemTrustDirectory = "/usr/local/share/ca-certificates"
const linuxSystemTrustBundle = "/etc/ssl/certs/ca-certificates.crt"

type linuxTrustRunner func(context.Context, string, ...string) ([]byte, error)

func installLocalTrust(ctx context.Context, _ Config, certificatePEM []byte) error {
	return installLinuxSystemTrust(ctx, linuxSystemTrustDirectory, linuxSystemTrustBundle, certificatePEM, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	})
}

func installLinuxSystemTrust(ctx context.Context, trustDirectory, trustBundle string, certificatePEM []byte, run linuxTrustRunner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	certificate, err := parseLocalTrustRoot(certificatePEM)
	if err != nil {
		return err
	}
	if err = protectedDirectory(trustDirectory, 0755); err != nil {
		return fmt.Errorf("prepare Linux system certificate directory: %w", err)
	}
	return installLinuxSystemTrustAt(ctx, trustDirectory, trustBundle, certificate, certificatePEM, 0, run)
}

func installLinuxSystemTrustAt(ctx context.Context, trustDirectory, trustBundle string, certificate *x509.Certificate, certificatePEM []byte, requiredUID uint32, run linuxTrustRunner) error {
	path := linuxOwnedTrustPath(trustDirectory, localTrustDomain(certificate))
	if err := writeLinuxSystemTrustFile(path, certificatePEM, requiredUID); err != nil {
		return err
	}
	if linuxTrustBundleContains(trustBundle, certificate.Raw) {
		return nil
	}
	output, err := run(ctx, "update-ca-certificates")
	if err != nil {
		return fmt.Errorf("install Paperboat HTTPS root in Linux system trust: %w: %s", err, trimTrustCommandOutput(output))
	}
	if !linuxTrustBundleContains(trustBundle, certificate.Raw) {
		return errors.New("Linux certificate update completed without activating the Paperboat HTTPS root")
	}
	return nil
}

func writeLinuxSystemTrustFile(path string, certificatePEM []byte, requiredUID uint32) error {
	if existing, err := readRootOwnedTrustFile(path, requiredUID); err == nil {
		if bytes.Equal(existing, certificatePEM) {
			return nil
		}
		return errors.New("Paperboat Linux trust path contains foreign material; preserved")
	} else if !os.IsNotExist(err) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".paperboat-trust-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err = temporary.Chmod(0644); err == nil {
		_, err = temporary.Write(certificatePEM)
	}
	if err == nil {
		err = temporary.Sync()
	}
	err = errors.Join(err, temporary.Close())
	if err != nil {
		return err
	}
	if err = os.Link(temporary.Name(), path); err != nil {
		if existing, readErr := readRootOwnedTrustFile(path, requiredUID); readErr == nil && bytes.Equal(existing, certificatePEM) {
			return nil
		}
		return fmt.Errorf("publish Paperboat Linux trust certificate: %w", err)
	}
	return nil
}

func readRootOwnedTrustFile(path string, requiredUID uint32) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != requiredUID || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("unsafe Paperboat Linux trust file; preserved")
	}
	return os.ReadFile(path)
}

func linuxTrustBundleContains(bundle string, rootDER []byte) bool {
	data, err := os.ReadFile(bundle)
	if err != nil {
		return false
	}
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		data = rest
		if block.Type == "CERTIFICATE" && bytes.Equal(block.Bytes, rootDER) {
			return true
		}
	}
	return false
}

func linuxOwnedTrustPath(directory, suffix string) string {
	digest := sha256.Sum256([]byte(suffix))
	return filepath.Join(directory, fmt.Sprintf("paperboat-machineguard-%x.crt", digest[:12]))
}

func trimTrustCommandOutput(output []byte) string {
	if len(output) > 512 {
		output = output[:512]
	}
	return strings.TrimSpace(string(output))
}

func removeLocalTrust(ctx context.Context, _ Config, certificatePEM []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	certificate, err := parseLocalTrustRoot(certificatePEM)
	if err != nil {
		return err
	}
	root := ownedRoot{suffix: localTrustDomain(certificate), pem: certificatePEM, certificate: certificate}
	return removeLinuxTrustFiles(ctx, []ownedRoot{root}, linuxSystemTrustDirectory, false, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	})
}

func cleanupHistoricalTrustExcept(ctx context.Context, cfg Config, activePEM []byte) error {
	retired, err := historicalRootsExcept(cfg.StateDir, activePEM)
	if err != nil || len(retired) == 0 {
		return err
	}
	return removeLinuxOwnedTrust(ctx, retired, linuxSystemTrustDirectory, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	})
}

func InstallUserTrust(ctx context.Context, certificatePEM []byte) error {
	return installLinuxUserTrust(ctx, certificatePEM, runNSSCommand)
}

func RemoveUserTrust(ctx context.Context, certificatePEM []byte) error {
	return removeLinuxUserTrust(ctx, certificatePEM, runNSSCommand)
}

func CleanupUserTrust(ctx context.Context) error {
	return cleanupLinuxUserTrust(ctx, runNSSCommand)
}

func ensureLocalTrustTools(ctx context.Context) error {
	if _, err := exec.LookPath("certutil"); err == nil {
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("installing Linux NSS certificate tools requires the privileged Paperboat installer")
	}
	distro, likes, err := linuxDistributionIDs()
	if err != nil {
		return err
	}
	ids := append([]string{distro}, likes...)
	var command string
	var args []string
	for _, id := range ids {
		switch strings.ToLower(id) {
		case "debian", "ubuntu", "linuxmint", "pop", "raspbian":
			command, args = "apt-get", []string{"install", "-y", "--no-upgrade", "--no-install-recommends", "libnss3-tools"}
		case "fedora", "rhel", "centos", "rocky", "almalinux", "amazon":
			command, args = "dnf", []string{"install", "-y", "nss-tools"}
		case "opensuse", "opensuse-leap", "opensuse-tumbleweed", "sles":
			command, args = "zypper", []string{"--non-interactive", "install", "mozilla-nss-tools"}
		case "arch", "archlinux", "manjaro":
			command, args = "pacman", []string{"--noconfirm", "--needed", "-S", "nss"}
		}
		if command != "" {
			break
		}
	}
	if command == "" {
		return fmt.Errorf("Linux browser trust requires NSS certutil; automatic prerequisite installation is unsupported for distribution %q", distro)
	}
	if _, err = exec.LookPath(command); err != nil {
		return fmt.Errorf("Linux browser trust requires NSS certutil; package manager %q is unavailable", command)
	}
	output, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("install Linux NSS browser trust tools: %w: %s", err, trimTrustCommandOutput(output))
	}
	if _, err = exec.LookPath("certutil"); err != nil {
		return errors.New("NSS certificate tools installed without the certutil executable")
	}
	return nil
}

func linuxDistributionIDs() (string, []string, error) {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return "", nil, fmt.Errorf("read Linux distribution metadata for NSS tools: %w", err)
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != "ID" && key != "ID_LIKE" {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, decodeErr := strconv.Unquote(value); decodeErr == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, "\"'")
		}
		values[key] = value
	}
	if err = scanner.Err(); err != nil {
		return "", nil, err
	}
	if values["ID"] == "" {
		return "", nil, errors.New("Linux distribution metadata has no ID")
	}
	return values["ID"], strings.Fields(values["ID_LIKE"]), nil
}

// NSS output consists of public certificate metadata, but its size and runtime
// are bounded even if a browser store is corrupt or contains excessive entries.
func runNSSCommand(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "certutil", args...)
	command.WaitDelay = time.Second
	output := &boundedNSSOutput{}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if output.exceeded {
		return output.bytes.Bytes(), errors.New("browser certificate command output exceeds limit")
	}
	return output.bytes.Bytes(), err
}

type boundedNSSOutput struct {
	bytes    bytes.Buffer
	exceeded bool
}

func (out *boundedNSSOutput) Write(data []byte) (int, error) {
	const limit = 1 << 20
	if len(data) > limit-out.bytes.Len() {
		out.exceeded = true
		return 0, errors.New("browser certificate command output exceeds limit")
	}
	return out.bytes.Write(data)
}
