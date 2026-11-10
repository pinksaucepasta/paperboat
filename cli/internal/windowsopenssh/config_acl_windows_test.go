//go:build windows

package windowsopenssh

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func init() {
	// On Windows the owner SID is parsed: text that is not exactly a SID is refused.
	forgedOwnerSIDs = append(forgedOwnerSIDs, "S-1-", "alice", "S-1-5-21-1)(A;;FA;;;WD)")
}

// Needs an elevated token: both files are owned by SYSTEM.
func TestManagedSSHFilesGetTheirOwnDACLAndAWidenedKeyFileIsRepaired(t *testing.T) {
	root := t.TempDir()
	owner := platformOwnerSID()
	keys := filepath.Join(root, "authorized_keys", "paperboat")
	if err := os.MkdirAll(filepath.Dir(keys), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keys, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wide, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)") // Everyone: full control
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := wide.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(keys, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}

	if err := protectManagedKeys(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.ProtectedDACLMatches(keys, "D:P(A;;FA;;;SY)(A;;FA;;;BA)") || !windowssecurity.OwnerMatchesSID(keys, system) {
		t.Fatal("the key file kept a wider ACL than SYSTEM and Administrators")
	}
	if body, err := os.ReadFile(keys); err != nil || string(body) != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample\n" {
		t.Fatalf("repairing the ACL changed the keys: %q, %v", body, err)
	}

	config, err := WriteServiceConfig(ServiceConfig{
		StateRoot: root, SSHDPath: filepath.Join(root, "sshd.exe"), SFTPPath: filepath.Join(root, "sftp-server.exe"),
		AuthorizedKeys: keys, ManagedUser: "alice", OwnerSID: owner, Port: 40001,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.ProtectedDACLMatches(config, generationFileDACL(owner)) || !windowssecurity.OwnerMatchesSID(config, system) {
		t.Fatal("sshd_config must be SYSTEM-owned: SYSTEM and Administrators full, the owner read, nothing inherited")
	}
}
