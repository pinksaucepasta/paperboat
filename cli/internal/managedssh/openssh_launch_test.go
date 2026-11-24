//go:build darwin || linux

package managedssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Regression 2026-11-24: nothing repaired a damaged managed configuration, whatever was damaged.
func TestOpenSSHConfigInstallRepairsDamage(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, directory string){
		"fragment deleted": func(t *testing.T, d string) { remove(t, filepath.Join(d, "paperboat_config")) },
		"fragment edited":  func(t *testing.T, d string) { rewrite(t, filepath.Join(d, "paperboat_config"), "Host x\n") },
		"include removed":  func(t *testing.T, d string) { rewrite(t, filepath.Join(d, "config"), "Host *\n    User me\n") },
		"include inside a Host block": func(t *testing.T, d string) {
			rewrite(t, filepath.Join(d, "config"), "Host *\n    User me\n"+openSSHIncludeLine)
		},
		"include indented in a Host block": func(t *testing.T, d string) {
			rewrite(t, filepath.Join(d, "config"), "Host *\n    User me\n    "+openSSHIncludeLine)
		},
		"config deleted":   func(t *testing.T, d string) { remove(t, filepath.Join(d, "config")) },
		"record corrupted": func(t *testing.T, d string) { rewrite(t, filepath.Join(d, ".paperboat-config-install-v1.json"), "{") },
		"record deleted":   func(t *testing.T, d string) { remove(t, filepath.Join(d, ".paperboat-config-install-v1.json")) },
	} {
		t.Run(name, func(t *testing.T) {
			home := openSSHTestHome(t)
			directory := filepath.Join(home, ".ssh")
			rewrite(t, filepath.Join(directory, "config"), "Host *\n    User me\n")
			config := openSSHTestConfig(home)
			config.Targets = []OpenSSHAliasTarget{{Alias: "box", User: "pbt", Port: 22}}
			if _, err := InstallOpenSSHConfig(config); err != nil {
				t.Fatal(err)
			}
			damage(t, directory)
			if err := ValidateInstalledOpenSSHConfig(home, config.OwnerUID, config.AgentSocket, config.AliasDomain); err == nil {
				t.Fatal("the damaged configuration passed the check")
			}
			if _, err := InstallOpenSSHConfig(config); err != nil {
				t.Fatalf("repair: %v", err)
			}
			if err := ValidateOpenSSHConfig(config); err != nil {
				t.Fatalf("after repair: %v", err)
			}
			main := readOpenSSHTestFile(t, filepath.Join(directory, "config"))
			if name != "config deleted" && !bytes.Contains(main, []byte("    User me\n")) {
				t.Fatalf("the user's own settings were lost: %q", main)
			}
			if replay, err := InstallOpenSSHConfig(config); err != nil || replay.Changed {
				t.Fatalf("replay=%+v err=%v", replay, err)
			}
			if _, err := UninstallOpenSSHConfig(home, uint32(os.Getuid())); err != nil {
				t.Fatalf("uninstall after repair: %v", err)
			}
		})
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func rewrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Regression 2026-11-24: pb ssh, scp, sftp and rsync started OpenSSH without Paperboat's transport and host keys.
func TestVerifyOpenSSHLaunch(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client is not installed")
	}
	home := openSSHTestHome(t)
	config := openSSHTestConfig(home)
	config.Targets = []OpenSSHAliasTarget{{Alias: "box", User: "pbt", Port: 22}}
	if _, err := InstallOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
	fragment := filepath.Join(home, ".ssh", "paperboat_config")
	host := "box." + testAliasDomain
	verify := func(arguments ...string) error {
		return VerifyOpenSSHLaunch(context.Background(), OpenSSHLaunch{Home: home, OwnerUID: config.OwnerUID, AgentSocket: config.AgentSocket,
			Domain: config.AliasDomain, Host: host, Executable: "ssh", Arguments: arguments, Environment: openSSHTestEnvironment(home)})
	}
	if err := verify("-F", fragment, "-o", "ConnectTimeout=5", "pbt@"+host, "true"); err != nil {
		t.Fatalf("pinned launch refused: %v", err)
	}
	for name, arguments := range map[string][]string{
		"no fragment":        {"-F", "/dev/null", "pbt@" + host},
		"proxy":              {"-F", fragment, "-o", "ProxyCommand=none", "pbt@" + host},
		"jump":               {"-F", fragment, "-J", "evil.example.test", "pbt@" + host},
		"no host check":      {"-F", fragment, "-o", "StrictHostKeyChecking=no", "pbt@" + host},
		"accept-new":         {"-F", fragment, "pbt@" + host, "-o", "StrictHostKeyChecking=accept-new", "true"},
		"own known hosts":    {"-F", fragment, "-o", "UserKnownHostsFile=/tmp/k", "pbt@" + host},
		"global known hosts": {"-F", fragment, "-o", "GlobalKnownHostsFile=/tmp/k", "pbt@" + host},
		"known hosts cmd":    {"-F", fragment, "-o", "KnownHostsCommand=/bin/true", "pbt@" + host},
		"host key alias":     {"-F", fragment, "-o", "HostKeyAlias=evil", "pbt@" + host},
		"host name":          {"-F", fragment, "-o", "Hostname=evil.example.test", "pbt@" + host},
		"dns keys":           {"-F", fragment, "-o", "VerifyHostKeyDNS=yes", "pbt@" + host},
	} {
		if err := verify(arguments...); !errors.Is(err, ErrOpenSSHLaunchUnpinned) {
			t.Errorf("%s: err=%v, want ErrOpenSSHLaunchUnpinned", name, err)
		}
	}
	remove(t, fragment)
	if err := verify("pbt@" + host); err == nil || errors.Is(err, ErrOpenSSHLaunchUnpinned) {
		t.Fatalf("missing fragment: err=%v, want a repairable error", err)
	}
}
