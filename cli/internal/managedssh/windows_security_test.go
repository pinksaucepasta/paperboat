//go:build windows

package managedssh

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func TestValidWindowsProfileRootStateAllowsOnlyUserOrSystemDirectory(t *testing.T) {
	const userSID = "S-1-5-21-1000"
	for _, test := range []struct {
		name    string
		mode    os.FileMode
		reparse bool
		owner   string
		want    bool
	}{
		{name: "current user", mode: os.ModeDir, owner: userSID, want: true},
		{name: "system", mode: os.ModeDir, owner: windowsSystemSID, want: true},
		{name: "administrators", mode: os.ModeDir, owner: "S-1-5-32-544"},
		{name: "foreign user", mode: os.ModeDir, owner: "S-1-5-21-2000"},
		{name: "reparse directory", mode: os.ModeDir, reparse: true, owner: userSID},
		{name: "symlink", mode: os.ModeSymlink, owner: userSID},
		{name: "regular file", mode: 0, owner: userSID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validWindowsProfileRootState(test.mode, test.reparse, test.owner, userSID); got != test.want {
				t.Fatalf("validWindowsProfileRootState() = %t, want %t", got, test.want)
			}
		})
	}
}

// Win32-OpenSSH's check_secure_file_permission (contrib/win32/win32compat/w32-sshfileperm.c): an owner of the user,
// SYSTEM, Administrators or TrustedInstaller is fine; no other principal may hold a write-class right.
func TestSSHPathSecurityProblemFollowsWin32OpenSSH(t *testing.T) {
	const userSID = "S-1-5-21-1-2-3-1000"
	const protected = "(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + userSID + ")"
	for _, test := range []struct {
		name, sddl, problem string
	}{
		{name: "owner is the user", sddl: "O:" + userSID + "D:P" + protected},
		{name: "owner is SYSTEM", sddl: "O:SYD:P" + protected},
		{name: "owner is Administrators (folder made from an elevated shell)", sddl: "O:BAD:P" + protected},
		{name: "owner is TrustedInstaller", sddl: "O:" + windowsTrustedInstallerID + "D:P" + protected},
		{name: "others may only read", sddl: "O:BAD:P" + protected + "(A;OICI;FR;;;BU)"},
		{name: "deny rules are fine", sddl: "O:BAD:P(D;;FA;;;WD)" + protected},
		{name: "owner is another user", sddl: "O:S-1-5-21-1-2-3-2000D:P" + protected, problem: "owned by S-1-5-21-1-2-3-2000"},
		{name: "Users can write", sddl: "O:BAD:P" + protected + "(A;OICI;FW;;;BU)", problem: "can change it"},
		{name: "another user can delete files inside", sddl: "O:" + userSID + "D:P" + protected + "(A;;DC;;;S-1-5-21-1-2-3-2000)", problem: "S-1-5-21-1-2-3-2000 can change it"},
		{name: "inheritable generic write", sddl: "O:" + userSID + "D:P" + protected + "(A;OICIIO;GW;;;WD)", problem: "can change it"},
		{name: "object rule", sddl: "O:" + userSID + "D:P" + protected + "(OA;;FR;bf967aba-0de6-11d0-a285-00aa003049e2;;WD)", problem: "cannot check"},
		{name: "no access list", sddl: "O:" + userSID + "D:NO_ACCESS_CONTROL", problem: "every user"},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatal(err)
			}
			owner, _, err := descriptor.Owner()
			if err != nil {
				t.Fatal(err)
			}
			dacl, _, err := descriptor.DACL()
			if err != nil {
				t.Fatal(err)
			}
			got := sshPathSecurityProblem(owner, dacl, userSID)
			if test.problem == "" && got != "" || test.problem != "" && !strings.Contains(got, test.problem) {
				t.Fatalf("problem = %q, want %q", got, test.problem)
			}
		})
	}
}

// The real path check accepts a .ssh folder an elevated shell made (owner Administrators) and refuses, with the
// folder and the cause named, one another user can write.
func TestVerifyCurrentUserOwnedPathAcceptsAdministratorsOwnedSSHFolder(t *testing.T) {
	sid, err := currentManagedSSHSID()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	set := func(sddl string) { setTestSecurity(t, directory, sddl) }
	protected := "(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + sid + ")"
	set("O:BAD:P" + protected)
	if err := verifyCurrentUserOwnedPath(directory, sid, true); err != nil {
		t.Fatalf("Administrators-owned .ssh refused: %v", err)
	}
	set("O:BAD:P" + protected + "(A;OICI;FW;;;WD)")
	var unsafePath *UnsafeSSHPathError
	if err := verifyCurrentUserOwnedPath(directory, sid, true); !errors.As(err, &unsafePath) || unsafePath.Path != directory || !strings.Contains(err.Error(), "can change it") || !errors.Is(err, ErrOpenSSHConfigConflict) {
		t.Fatalf("writable .ssh error = %v", err)
	}
}

func setTestSecurity(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, _ := descriptor.Owner()
	dacl, _, _ := descriptor.DACL()
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
	if errors.Is(err, windows.ERROR_INVALID_OWNER) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skip("setting an Administrators owner needs an elevated test process")
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A ~/.ssh/config made from an elevated shell (owner Administrators) is read, as Windows OpenSSH reads it, and
// Paperboat can still rewrite it from an unelevated process; one another user can write is refused by name.
func TestUserSSHConfigOwnedByAdministratorsIsReadAndRewritten(t *testing.T) {
	sid, err := currentManagedSSHSID()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "config")
	if err := os.WriteFile(path, []byte("Host example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	protected := "(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")"
	setTestSecurity(t, path, "O:BAD:P"+protected)
	if value, exists, err := readWindowsSSHFile(directory, "config", sid, false); err != nil || !exists || string(value) != "Host example\n" {
		t.Fatalf("Administrators-owned config: value=%q exists=%v err=%v", value, exists, err)
	}
	next := []byte("Host example\nInclude ~/.ssh/paperboat_config\n")
	if err := windowssecurity.WithUnelevatedToken(func() error { return replaceWindowsUserSSHFile(directory, path, sid, next) }); err != nil {
		t.Fatalf("unelevated rewrite of an Administrators-owned config: %v", err)
	}
	if value, _, err := readWindowsSSHFile(directory, "config", sid, false); err != nil || string(value) != string(next) {
		t.Fatalf("rewritten config: value=%q err=%v", value, err)
	}
	setTestSecurity(t, path, "O:BAD:P"+protected+"(A;;FW;;;WD)")
	var unsafePath *UnsafeSSHPathError
	if _, _, err := readWindowsSSHFile(directory, "config", sid, false); !errors.As(err, &unsafePath) || unsafePath.Path != path || !strings.Contains(err.Error(), "can change it") {
		t.Fatalf("writable config error = %v", err)
	}
}
