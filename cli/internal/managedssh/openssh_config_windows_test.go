//go:build windows

package managedssh

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsOpenSSHOptionConflictOmitsConfiguredCommandValues(t *testing.T) {
	err := &OpenSSHOptionConflict{Line: 12, Option: "KnownHostsCommand", Existing: `C:\private-user\pb.exe --token secret`, Required: `C:\Program Files\Paperboat\pb.exe --known-hosts`}
	if !errors.Is(err, ErrOpenSSHConfigConflict) {
		t.Fatalf("conflict sentinel lost: %v", err)
	}
	for _, private := range []string{"private-user", "secret", "known-hosts"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("conflict error exposed configured value %q: %q", private, err)
		}
	}
	var classified interface {
		DiagnosticStage() string
		DiagnosticCode() string
	}
	if !errors.As(err, &classified) || classified.DiagnosticStage() != "command" || classified.DiagnosticCode() != "managed_ssh_failed" {
		t.Fatalf("conflict classification missing: %#v", err)
	}
}

func TestValidateInstalledOpenSSHConfigAcceptsTargetBlocks(t *testing.T) {
	home := managedSSHWindowsTestHome(t)
	agentSocket := `\\.\pipe\paperboat-ssh-agent-test`
	config := OpenSSHConfig{AliasDomain: testAliasDomain,
		Home:               home,
		ProxyCommand:       `"C:\Program Files\Paperboat\bin\pb.exe" __ssh-proxy --host %h --port %p --user %r`,
		KnownHostsCommand:  `"C:\Program Files\Paperboat\bin\pb.exe" __ssh-known-hosts --host %h --port %p`,
		AgentSocket:        agentSocket,
		IdentityFile:       ManagedIdentityPublicKeyPath(home),
		CertificateCommand: "//?/C:/PROGRA~1/PAPERB~1/bin/pb.exe",
		Targets: []OpenSSHAliasTarget{
			{Alias: "hn", User: "root", Port: 22},
			{Alias: "win-target", User: "Tester", Port: 38222},
		},
	}
	if _, err := InstallOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInstalledOpenSSHConfig(home, 0, agentSocket, testAliasDomain); err != nil {
		t.Fatalf("target-bearing installed config was rejected: %v", err)
	}
	if err := ValidateInstalledOpenSSHConfig(home, 0, `\\.\pipe\paperboat-ssh-agent-other`, testAliasDomain); !errors.Is(err, ErrOpenSSHConfigConflict) {
		t.Fatalf("mismatched agent socket error=%v", err)
	}
}

func TestUninstallOpenSSHConfigIgnoresUnmanagedSSHDirectory(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("example.invalid ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := UninstallOpenSSHConfig(home, 0)
	if err != nil || result.Changed {
		t.Fatalf("uninstall unmanaged directory: result=%+v error=%v", result, err)
	}
	if _, err := os.Stat(knownHosts); err != nil {
		t.Fatalf("unmanaged known_hosts changed: %v", err)
	}
}

func TestUninstallManagedIdentityIgnoresUnmanagedSSHDirectory(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("example.invalid ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UninstallManagedIdentityPublicKey(home, 0); err != nil {
		t.Fatalf("uninstall absent managed identity: %v", err)
	}
	if _, err := os.Stat(knownHosts); err != nil {
		t.Fatalf("unmanaged known_hosts changed: %v", err)
	}
}

func TestUninstallOpenSSHConfigDoesNotIgnorePartialManagedState(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config"), []byte("Host *\n# "+openSSHIncludeMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := UninstallOpenSSHConfig(home, 0); !errors.Is(err, ErrOpenSSHConfigConflict) {
		t.Fatalf("partial managed state error=%v", err)
	}
}

// A user moved pb's Include line under a Host block: OpenSSH then applies it to that host only.
func TestWindowsOpenSSHConfigMovedIncludeIsRepaired(t *testing.T) {
	const line = "Include ~/.ssh/paperboat_config # paperboat-managed-ssh-include-v1\n"
	for _, test := range []struct{ name, moved, repaired, removed string }{
		{"nested", "# top\nHost x\n  User a\n  " + line + "  Port 2\n", "# top\n" + line + "Host x\n  User a\n  Port 2\n", "# top\nHost x\n  User a\n  Port 2\n"},
		{"equals", "Host=x\n" + line, line + "Host=x\n", "Host=x\n"},
		{"commented", "# top\n# " + line + "Host x\n", "# top\n# " + line + "Host x\n", "# top\nHost x\n"},
	} {
		for _, repair := range []bool{true, false} {
			home, config := managedSSHWindowsTestHome(t), OpenSSHConfig{}
			config = managedSSHWindowsTestConfig(home)
			path := filepath.Join(home, ".ssh", "config")
			read := func() string { value, _ := os.ReadFile(path); return string(value) }
			write := func(value string) {
				if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InstallOpenSSHConfig(config); err != nil {
				t.Fatal(err)
			}
			write(test.moved)
			if err := ValidateOpenSSHConfig(config); !errors.Is(err, ErrOpenSSHConfigConflict) {
				t.Fatalf("%s: validate=%v", test.name, err)
			}
			if repair {
				if _, err := InstallOpenSSHConfig(config); err != nil || read() != test.repaired {
					t.Fatalf("%s: repair=%v config=%q", test.name, err, read())
				}
			}
			if _, err := UninstallOpenSSHConfig(home, config.OwnerUID); err != nil || read() != test.removed {
				t.Fatalf("%s: uninstall=%v config=%q", test.name, err, read())
			}
		}
	}
}
