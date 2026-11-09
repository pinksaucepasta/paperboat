//go:build windows

package hostinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

// A retired image is removed as soon as nothing holds it; one still in use stays and is counted.
func TestSweepWindowsRetiredImagesRemovesWhatIsNoLongerInUse(t *testing.T) {
	releases := filepath.Join(t.TempDir(), "releases")
	pinned := filepath.Join(releases, "versions", "custom-1")
	if err := os.MkdirAll(pinned, 0o700); err != nil {
		t.Fatal(err)
	}
	free := filepath.Join(releases, "pb.rollback.exe"+windowsRetiredSuffix+"a1")
	held := filepath.Join(pinned, "pb.exe.rollback"+windowsRetiredSuffix+"b2")
	active := filepath.Join(pinned, "pb.exe")
	for _, path := range []string{free, held, active} {
		if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	name, err := windows.UTF16PtrFromString(held)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0) // no sharing: cannot be deleted
	if err != nil {
		t.Fatal(err)
	}
	layout := service.Layout{ReleasesRoot: releases}
	if left := SweepWindowsRetiredImages(layout); left != 1 {
		windows.CloseHandle(handle)
		t.Fatalf("still in use = %d, want 1", left)
	}
	if _, err := os.Lstat(free); !os.IsNotExist(err) {
		t.Fatalf("an unused retired image was kept: %v", err)
	}
	windows.CloseHandle(handle)
	if left := SweepWindowsRetiredImages(layout); left != 0 {
		t.Fatalf("still in use after release = %d, want 0", left)
	}
	if _, err := os.Lstat(active); err != nil {
		t.Fatalf("the active executable was touched: %v", err)
	}
}

// Only the exact names the updater creates are removed, and nothing is reached through a junction.
func TestSweepWindowsRetiredImagesKeepsOtherNamesAndDoesNotFollowJunctions(t *testing.T) {
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	outside := filepath.Join(root, "outside")
	for _, directory := range []string{filepath.Join(releases, "versions"), outside} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lookalike := filepath.Join(releases, "notes.retired-2026.txt")
	victim := filepath.Join(outside, "pb.exe.rollback"+windowsRetiredSuffix+"c3")
	for _, path := range []string{lookalike, victim} {
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	junction := filepath.Join(releases, "versions", "custom-linked")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, outside).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, out)
	}
	if left := SweepWindowsRetiredImages(service.Layout{ReleasesRoot: releases}); left != 0 {
		t.Fatalf("still in use = %d, want 0", left)
	}
	for _, path := range []string{lookalike, victim} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(path), err)
		}
	}
}
