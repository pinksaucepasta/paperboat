package windowsopenssh

import (
	"context"
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
				AuthorizedKeys: filepath.Join(root, "managed"), ManagedUser: identity, OwnerSID: testOwnerSID, Port: 40001,
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
				AuthorizedKeys: filepath.Join(root, "managed"), ManagedUser: identity, OwnerSID: testOwnerSID, Port: 40001,
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Win32-OpenSSH names a domain account domain\user in lowercase; OpenSSH
			// keeps a backslash before a letter literally, so it is never doubled.
			pattern := `"` + strings.ToLower(identity) + `"`
			for _, line := range []string{"\nMatch User " + pattern + " Group administrators\n", "\nMatch User " + pattern + "\n"} {
				if !strings.Contains(string(body), line) {
					t.Fatalf("Windows SSH identities must remain one exact, lowercase Match User argument:\n%s", body)
				}
			}
			managed := `"` + filepath.Join(root, "managed") + `"`
			if got := effectiveSSHD(string(body), strings.ToLower(identity), true)["authorizedkeysfile"]; !strings.HasPrefix(got, managed+" ") {
				t.Fatalf("%s does not read the managed key file: %q", identity, got)
			}
		})
	}
}

const testOwnerSID = "S-1-5-21-1-2-3-1001"

var forgedOwnerSIDs = []string{""}

func TestWriteServiceConfigRefusesAMissingOwner(t *testing.T) {
	for _, owner := range forgedOwnerSIDs {
		root := t.TempDir()
		_, err := WriteServiceConfig(ServiceConfig{
			StateRoot: root, SSHDPath: filepath.Join(root, "sshd.exe"), SFTPPath: filepath.Join(root, "sftp-server.exe"),
			AuthorizedKeys: filepath.Join(root, "managed"), ManagedUser: "alice", OwnerSID: owner, Port: 40001,
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("owner %q: configuration written: %v", owner, err)
		}
	}
}

// An owner SID that no longer resolves (deleted account, foreign SID) must stop
// setup before anything is written: no key file, no configuration naming it.
func TestSetupWithAnUnresolvableOwnerWritesNothing(t *testing.T) {
	root := t.TempDir()
	config := Config{InstallRoot: filepath.Join(root, "install"), StateRoot: filepath.Join(root, "state"), Port: 38222, OwnerSID: "S-1-5-21-1-2-3-424242"}
	if _, err := prepareStateFromResult(context.Background(), config, resultForConfig(config)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("setup with an unresolvable owner = %v, want ErrInvalidConfig", err)
	}
	if _, err := os.Stat(config.StateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup wrote state for an unresolvable owner: %v", err)
	}
}

func TestManagedUserForSIDNeverFallsBackToTheServiceAccount(t *testing.T) {
	if _, err := managedUserForSID("S-1-5-21-not-an-account"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("an unresolved owner must not become the current service account: %v", err)
	}
}

func TestWriteServiceConfigReplacesAGlobalManagedKeyFile(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "authorized_keys", "paperboat")
	// The configuration every build before the key scope wrote.
	old := "Port 40001\nPasswordAuthentication no\nAuthorizedKeysFile \"" + managed + "\"\n"
	if err := os.WriteFile(filepath.Join(root, "sshd_config"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := WriteServiceConfig(ServiceConfig{
		StateRoot: root, SSHDPath: filepath.Join(root, "sshd.exe"), SFTPPath: filepath.Join(root, "sftp-server.exe"),
		AuthorizedKeys: managed, ManagedUser: "alice", OwnerSID: testOwnerSID, Port: 40001,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := effectiveSSHD(string(body), "bob", false)["authorizedkeysfile"]; got != ".ssh/authorized_keys" {
		t.Fatalf("another account reads %q after an upgrade", got)
	}
}

// effectiveSSHD resolves the keywords this package writes the way sshd does for
// one connection: a matching Match block's first value beats the global one.
// Criteria are the two Paperboat emits, User "<name>" and Group administrators.
func effectiveSSHD(config, user string, administrator bool) map[string]string {
	global, matched := map[string]string{}, map[string]string{}
	active, inMatch := true, false
	for _, line := range strings.Split(config, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		key = strings.ToLower(key)
		if key == "" || strings.HasPrefix(key, "#") {
			continue
		}
		if key == "match" {
			inMatch, active = true, true
			for rest := value; rest != ""; {
				var criterion, argument string
				criterion, rest, _ = strings.Cut(rest, " ")
				if strings.HasPrefix(rest, `"`) { // one quoted argument may hold spaces
					end := strings.Index(rest[1:], `"`) + 2
					argument, rest = rest[:end], strings.TrimSpace(rest[end:])
				} else {
					argument, rest, _ = strings.Cut(rest, " ")
				}
				switch criterion {
				case "User":
					active = active && argument == `"`+user+`"`
				case "Group":
					active = active && argument == "administrators" && administrator
				default:
					active = false
				}
			}
			continue
		}
		target := global
		if inMatch {
			target = matched
		}
		if _, set := target[key]; active && !set {
			target[key] = value
		}
	}
	for key, value := range matched {
		global[key] = value
	}
	if _, set := global["passwordauthentication"]; !set {
		global["passwordauthentication"] = "yes" // OpenSSH's default
	}
	return global
}
