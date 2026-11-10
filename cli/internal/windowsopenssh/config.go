package windowsopenssh

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

type ServiceConfig struct {
	StateRoot      string
	HostKey        string
	SSHDPath       string
	SFTPPath       string
	AuthorizedKeys string
	ManagedUser    string
	OwnerSID       string
	Port           uint16
}

func WriteServiceConfig(config ServiceConfig) (string, error) {
	if config.HostKey == "" {
		config.HostKey = filepath.Join(config.StateRoot, "hostkeys", "ssh_host_ed25519_key")
	}
	if config.Port == 0 || !validOwnerSID(config.OwnerSID) || !validManagedUser(config.ManagedUser) {
		return "", ErrInvalidConfig
	}
	for _, path := range []string{config.StateRoot, config.SSHDPath, config.SFTPPath, config.AuthorizedKeys, config.HostKey} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\"\r\n\x00") {
			return "", ErrInvalidConfig
		}
	}
	if config.HostKey == filepath.Join(config.StateRoot, "hostkeys", "ssh_host_ed25519_key") {
		if err := os.MkdirAll(filepath.Join(config.StateRoot, "hostkeys"), 0o700); err != nil {
			return "", operationFailure("could not create the managed OpenSSH host-key directory", nil, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(config.StateRoot, "logs"), 0o700); err != nil {
		return "", operationFailure("could not create the managed OpenSSH log directory", nil, err)
	}
	quote := func(value string) string { return `"` + value + `"` }
	// Win32-OpenSSH matches lowercase names, domain accounts as domain\user;
	// OpenSSH keeps a backslash before a letter literally, quoted or not.
	user, managed := quote(strings.ToLower(config.ManagedUser)), quote(config.AuthorizedKeys)
	const administratorKeys = "__PROGRAMDATA__/ssh/administrators_authorized_keys"
	lines := []string{
		"# Managed by Paperboat. Manual changes are replaced.",
		"Port " + strconv.Itoa(int(config.Port)),
		"ListenAddress 127.0.0.1",
		"ListenAddress ::1",
		"Protocol 2",
		// No global PasswordAuthentication: stock Windows OpenSSH leaves it commented,
		// so OpenSSH's default (yes) lets every other account keep its password, as
		// its own sshd does on Linux and macOS (F5-11). The setup account is key-only below.
		"KbdInteractiveAuthentication no",
		"PermitEmptyPasswords no",
		"PubkeyAuthentication yes",
		"AuthorizedKeysFile .ssh/authorized_keys",
		"HostKey " + quote(config.HostKey),
		// Port and agent forwarding follow OpenSSH's defaults, as on Linux and macOS; X11 stays off.
		"GatewayPorts no",
		"PermitTunnel no",
		"X11Forwarding no",
		"Subsystem sftp " + quote(config.SFTPPath),
		"LogLevel VERBOSE",
		// The first Match that sets a keyword wins (sshd_config(5)). Only the setup
		// account reads Paperboat's key file, next to the file stock Windows OpenSSH
		// gives it (administrators_authorized_keys for an administrator), and keeps
		// the key-only policy it had. Other accounts keep the stock rules below.
		"Match User " + user + " Group administrators",
		"    AuthorizedKeysFile " + managed + " " + administratorKeys,
		"    PasswordAuthentication no",
		"Match User " + user,
		"    AuthorizedKeysFile " + managed + " .ssh/authorized_keys",
		"    PasswordAuthentication no",
		"Match Group administrators",
		"    AuthorizedKeysFile " + administratorKeys,
	}
	path := filepath.Join(config.StateRoot, "sshd_config")
	if err := atomicfile.Write(path, []byte(strings.Join(lines, "\n")+"\n"), serviceConfigFileOptions(config.OwnerSID)); err != nil {
		return "", operationFailure("could not write the managed OpenSSH server configuration", nil, err)
	}
	return path, nil
}

func validManagedUser(name string) bool {
	if name == "" || strings.ContainsAny(name, "\"'*,?![]/\r\n\t\x00") || strings.Count(name, `\`) > 1 {
		return false
	}
	for _, part := range strings.Split(name, `\`) {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}

// The service runs as SYSTEM, so its current user must never choose whose
// account receives the managed key. Resolve the recorded installation owner.
func managedUserForSID(ownerSID string) (string, error) {
	account, err := user.LookupId(ownerSID)
	if err != nil {
		return "", operationFailure("could not resolve the Windows SSH setup account; rerun pb install as that account", ErrInvalidConfig, err)
	}
	name := account.Username
	if domain, local, qualified := strings.Cut(name, `\`); qualified {
		host, err := platformSSHComputerName()
		if err != nil {
			return "", operationFailure("could not identify the Windows SSH setup account's computer; retry pb install", ErrInvalidConfig, err)
		}
		if strings.EqualFold(domain, host) {
			name = local
		}
	}
	if !validManagedUser(name) {
		return "", operationFailure("the Windows SSH setup account name cannot be used as an exact SSH user; use an account without SSH pattern characters", ErrInvalidConfig, nil)
	}
	return strings.ToLower(name), nil
}

func ValidateServiceConfig(runner Runner, sshdPath, configPath string) error {
	if runner == nil || !filepath.IsAbs(sshdPath) || !filepath.IsAbs(configPath) {
		return ErrInvalidConfig
	}
	validationCtx, cancel := context.WithTimeout(contextBackground(), 15*time.Second)
	defer cancel()
	_, err := runner.Run(validationCtx, sshdPath, "-t", "-f", configPath)
	if err != nil {
		return operationFailure("Windows OpenSSH rejected the managed server configuration; check its directives and approved executable", ErrInvalidConfig, err)
	}
	return nil
}

var contextBackground = func() context.Context { return context.Background() }
