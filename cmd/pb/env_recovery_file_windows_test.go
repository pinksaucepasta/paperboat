//go:build windows

package main

import (
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvironmentRecoveryFileProtectedBeforeWrite(t *testing.T) {
	parent := t.TempDir()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "recovery.txt")
	file, err := createEnvironmentRecoveryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	expected := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if sid != "S-1-5-18" {
		expected += "(A;;FA;;;" + sid + ")"
	}
	handle := windows.Handle(file.Fd())
	if !windowssecurity.HandleOwnerMatchesSID(handle, user.User.Sid) || !windowssecurity.ProtectedHandleDACLMatches(handle, expected) {
		t.Fatal("export lacks protected owner ACL")
	}
	if info, err := file.Stat(); err != nil || info.Size() != 0 {
		t.Fatal("creation wrote data before ACL validation")
	}
	if _, err := file.WriteString("test-only-recovery\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if another, err := createEnvironmentRecoveryFile(path); err == nil {
		another.Close()
		t.Fatal("overwrote existing export")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "test-only-recovery\n" {
		t.Fatal("existing export changed")
	}
}

func TestEnvironmentRecoveryFileRejectsReparseParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("Windows symlink privilege unavailable")
	}
	if file, err := createEnvironmentRecoveryFile(filepath.Join(link, "recovery.txt")); err == nil {
		file.Close()
		t.Fatal("accepted reparse parent")
	}
	if _, err := os.Stat(filepath.Join(target, "recovery.txt")); !os.IsNotExist(err) {
		t.Fatal("created export through reparse parent")
	}
}
