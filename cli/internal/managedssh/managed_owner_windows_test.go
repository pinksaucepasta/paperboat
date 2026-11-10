//go:build windows

package managedssh

import (
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// An elevated administrator's token has BUILTIN\\Administrators as its default owner. The managed SSH state the
// daemon writes on every start must still be owned by the user, with the exact protected DACL, so the owner-strict
// reads that follow accept it.
func TestWindowsManagedSSHStateIsUserOwnedUnderAnAdministratorsDefaultOwner(t *testing.T) {
	home := managedSSHWindowsTestHome(t)
	publicKey := authorizedPublicLine(t)
	config := managedSSHWindowsTestConfig(home)

	withAdministratorsDefaultOwner(t, func() error {
		if err := InstallManagedIdentityPublicKey(home, 0, publicKey); err != nil {
			return err
		}
		_, err := InstallOpenSSHConfig(config)
		return err
	})
	assertWindowsManagedSSHStateOwner(t, home, currentWindowsUserSID(t))
	if err := ValidateManagedIdentityPublicKey(home, 0, publicKey); err != nil {
		t.Fatalf("validate identity: %v", err)
	}
	if err := ValidateInstalledOpenSSHConfig(home, 0, config.AgentSocket, testAliasDomain); err != nil {
		t.Fatalf("validate OpenSSH config: %v", err)
	}
}

func managedSSHWindowsTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	user := currentWindowsUserSID(t)
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(home, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user, nil, nil, nil)
	}); err != nil {
		t.Skipf("setting an exact Windows test-home owner requires SeRestorePrivilege: %v", err)
	}
	return home
}

func managedSSHWindowsTestConfig(home string) OpenSSHConfig {
	return OpenSSHConfig{AliasDomain: testAliasDomain,
		Home:              home,
		ProxyCommand:      `"C:\Program Files\Paperboat\bin\pb.exe" __ssh-proxy --host %h --port %p --user %r`,
		KnownHostsCommand: `"C:\Program Files\Paperboat\bin\pb.exe" __ssh-known-hosts --host %h --port %p`,
		AgentSocket:       `\\.\pipe\paperboat-ssh-agent-test`,
		IdentityFile:      ManagedIdentityPublicKeyPath(home),
		Targets:           []OpenSSHAliasTarget{{Alias: "hn", User: "root", Port: 22}},
	}
}

func currentWindowsUserSID(t *testing.T) *windows.SID {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		t.Fatal(err)
	}
	return user.User.Sid
}

func withAdministratorsDefaultOwner(t *testing.T, operation func() error) {
	t.Helper()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	member, err := windows.GetCurrentProcessToken().IsMember(administrators)
	if err != nil || !member {
		t.Skip("Administrators-default-owner regression requires an elevated administrator token")
	}
	if err := windowssecurity.WithRestorePrivilegeAndOwner(administrators, operation); err != nil {
		t.Fatal(err)
	}
}

func assertWindowsManagedSSHStateOwner(t *testing.T, home string, owner *windows.SID) {
	t.Helper()
	sid := owner.String()
	for _, name := range []string{"config", "paperboat_config", ".paperboat-config-install-v1.json", ManagedIdentityPublicKeyFilename} {
		path := filepath.Join(home, ".ssh", name)
		if !windowsOwnerMatches(path, owner) {
			t.Errorf("%s owner is not %s", name, sid)
		}
		if err := verifyManagedSSHACL(path, sid); err != nil {
			t.Errorf("%s owner/DACL verification: %v", name, err)
		}
	}
}

func windowsOwnerMatches(path string, owner *windows.SID) bool {
	return windowssecurity.OwnerMatchesSID(path, owner)
}
