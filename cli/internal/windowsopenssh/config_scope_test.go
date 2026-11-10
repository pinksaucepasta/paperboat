package windowsopenssh

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteServiceConfigRefusesManagedUserPatterns(t *testing.T) {
	for name, identity := range map[string]string{
		"empty": "", "wildcard": "*", "list": "owner,other", "negated": "!other",
		"question": "ow?er", "bracket": "[owner]", "newline": "owner\nMatch All",
		"quote": "owner\" other", "control": "owner\x01", "empty domain": `\owner`,
		"empty account": `domain\`, "two domains": `domain\nested\owner`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			_, err := WriteServiceConfig(ServiceConfig{
				StateRoot: root, SSHDPath: filepath.Join(root, "sshd.exe"),
				SFTPPath:       filepath.Join(root, "sftp-server.exe"),
				AuthorizedKeys: filepath.Join(root, "managed"), ManagedUser: identity, Port: 40001,
			})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("unsafe managed user accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "sshd_config")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("an invalid user must not publish an SSH server configuration")
			}
		})
	}
}

func TestWriteServiceConfigKeepsAnExactWindowsUserName(t *testing.T) {
	for _, identity := range []string{`CONTOSO\Alice`, "Alice Smith", "船长"} {
		t.Run(identity, func(t *testing.T) {
			root := t.TempDir()
			path, err := WriteServiceConfig(ServiceConfig{
				StateRoot: root, SSHDPath: filepath.Join(root, "sshd.exe"),
				SFTPPath:       filepath.Join(root, "sftp-server.exe"),
				AuthorizedKeys: filepath.Join(root, "managed"), ManagedUser: identity, Port: 40001,
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "\nMatch User \""+strings.ToLower(identity)+"\"\n") {
				t.Fatal("Windows SSH identities must remain one exact, lowercase Match User argument")
			}
		})
	}
}

func TestManagedUserForSIDNeverFallsBackToTheServiceAccount(t *testing.T) {
	if _, err := managedUserForSID("S-1-5-21-not-an-account"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("an unresolved owner must not become the current service account: %v", err)
	}
}
