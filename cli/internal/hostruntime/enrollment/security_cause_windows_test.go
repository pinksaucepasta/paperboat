//go:build windows

package enrollment

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWindowsEnrollmentCustodyProbeCauseAndRecovery(t *testing.T) {
	checks := []struct {
		name  string
		check func(string, os.FileInfo, int64) (bool, error)
	}{
		{"configuration", checkEnrollmentConfigFile}, {"identity", checkIdentityFile},
	}
	for _, item := range checks {
		t.Run(item.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "protected.json")
			write := func() {
				t.Helper()
				if err := os.WriteFile(path, []byte("protected"), 0o600); err != nil {
					t.Fatal("write fixture failed")
				}
				if err := makeEnrollmentConfigPrivate(path); err != nil {
					t.Fatal("protect fixture failed")
				}
			}
			write()
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal("stat fixture failed")
			}
			if matches, err := item.check(path, info, 1024); err != nil || !matches {
				t.Fatal("valid protected file rejected")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal("remove fixture failed")
			}
			matches, err := item.check(path, info, 1024)
			if matches || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
				t.Fatal("disappeared object probe lost native cause")
			}
			if item.name == "configuration" && secureEnrollmentConfigFile(path, info, 1024) {
				t.Fatal("configuration boolean did not fail closed")
			}
			if item.name == "identity" && secureIdentityFile(path, info, 1024) {
				t.Fatal("identity boolean did not fail closed")
			}
			if matches, err := item.check(path+"\x00", info, 1024); matches || !errors.Is(err, syscall.EINVAL) {
				t.Fatal("invalid native path lost encoding cause")
			}
			write()
			info, err = os.Lstat(path)
			if err != nil {
				t.Fatal("recovery stat failed")
			}
			if matches, err := item.check(path, info, 1024); err != nil || !matches {
				t.Fatal("fresh protected file did not recover")
			}
		})
	}
}

func TestWindowsEnrollmentCustodyPreservesSingleLinkAndReparsePolicy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "protected.json")
	if err := os.WriteFile(path, []byte("protected"), 0o600); err != nil {
		t.Fatal("write fixture failed")
	}
	if err := makeEnrollmentConfigPrivate(path); err != nil {
		t.Fatal("protect fixture failed")
	}
	linked := filepath.Join(root, "linked.json")
	if err := os.Link(path, linked); err != nil {
		t.Fatal("native hardlink fixture unavailable")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal("stat fixture failed")
	}
	for _, check := range []func(string, os.FileInfo, int64) (bool, error){checkEnrollmentConfigFile, checkIdentityFile} {
		if matches, err := check(path, info, 1024); err != nil || matches {
			t.Fatal("hardlink custody policy changed")
		}
	}
	if err := os.Remove(linked); err != nil {
		t.Fatal("unlink fixture failed")
	}
	symlink := filepath.Join(root, "reparse.json")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal("native reparse fixture unavailable")
	}
	linkInfo, err := os.Lstat(symlink)
	if err != nil {
		t.Fatal("stat reparse failed")
	}
	for _, check := range []func(string, os.FileInfo, int64) (bool, error){checkEnrollmentConfigFile, checkIdentityFile} {
		if matches, err := check(symlink, linkInfo, 1024); err != nil || matches {
			t.Fatal("reparse custody policy changed")
		}
		if matches, err := check(path, info, 1); err != nil || matches {
			t.Fatal("file size bound changed")
		}
		fresh, err := os.Lstat(path)
		if err != nil {
			t.Fatal("stat recovery failed")
		}
		if matches, err := check(path, fresh, 1024); err != nil || !matches {
			t.Fatal("custody did not recover after hardlink removal")
		}
	}
}
