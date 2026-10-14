//go:build windows

package endpointbinary

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func TestResolveExecutableThroughProtectedAncestor(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "bin")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "pb.exe")
	if err := os.WriteFile(executable, []byte("executable fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	// Like the installed bin directory and executable, children retain their
	// explicit enrolled-user rights when the administrative parent is protected.
	for _, child := range []string{directory, executable} {
		security, err := windows.GetNamedSecurityInfo(child, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := security.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(child, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
			t.Fatal(err)
		}
		runtime.KeepAlive(security)
	}
	original, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	originalDACL, _, err := original.DACL()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, originalDACL, nil); err != nil {
			t.Error(err)
		}
		runtime.KeepAlive(original)
	}()
	// Executing a known file requires traversal, not listing its ancestors.
	// Restrict this parent to SYSTEM, leaving the enrolled user's child access intact.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	runtime.LockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	defer func() {
		if err := windows.RevertToSelf(); err != nil {
			t.Errorf("restore test thread identity: %v", err)
			return
		}
		runtime.UnlockOSThread()
	}()
	var token windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_PRIVILEGES, false, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	if err := windows.AdjustTokenPrivileges(token, true, nil, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	var privileges windows.Tokenprivileges
	privileges.PrivilegeCount = 1
	name, _ := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
	if err := windows.LookupPrivilegeValue(nil, name, &privileges.Privileges[0].Luid); err != nil {
		t.Fatal(err)
	}
	privileges.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	if err := windows.AdjustTokenPrivileges(token, false, &privileges, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.EvalSymlinks(executable); err == nil {
		t.Fatal("fixture did not prohibit ancestor enumeration")
	}
	got, err := CLI(executable)
	if err != nil {
		t.Fatalf("resolve readable executable through non-enumerable ancestor: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("resolved relative path %q", got)
	}
	before, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(got)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("resolved different executable: %q, %v", got, err)
	}
}

func TestResolveWindowsExecutableLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "actual.exe")
	link := filepath.Join(root, "pb.exe")
	if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := CLI(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("resolved link = %q, want %q", got, target)
	}
}
