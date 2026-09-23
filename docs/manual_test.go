package climan

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestManualInstallRepairAndRemovePreservesUnownedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Install(dir); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(dir, "man1", "pb.1")
	want, err := os.ReadFile(page)
	if err != nil || !bytes.HasPrefix(want, []byte(manualMarker)) {
		t.Fatalf("page: %v", err)
	}
	for name, data := range map[string]string{"other.1": "unrelated", "pb-personal.1": "user-owned", "pb-removed.1": manualMarker + "old page"} {
		if err := os.WriteFile(filepath.Join(dir, "man1", name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(page, []byte("interrupted old page"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Install(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(page)
	if !bytes.Equal(got, want) {
		t.Fatal("manual was not repaired")
	}
	if _, err := os.Stat(filepath.Join(dir, "man1", "pb-removed.1")); !os.IsNotExist(err) {
		t.Fatalf("obsolete owned page: %v", err)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"other.1", "pb-personal.1"} {
		if _, err := os.Stat(filepath.Join(dir, "man1", name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(page); !os.IsNotExist(err) {
		t.Fatalf("owned page retained: %v", err)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
}

func TestManualInstallRejectsSymlinks(t *testing.T) {
	for _, kind := range []string{"section", "page"} {
		t.Run(kind, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			if kind == "section" {
				if err := os.Symlink(outside, filepath.Join(dir, "man1")); err != nil {
					t.Skip(err)
				}
			} else {
				if err := os.Mkdir(filepath.Join(dir, "man1"), 0755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(outside, "target")
				if err := os.WriteFile(target, []byte("preserve"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, "man1", "pb.1")); err != nil {
					t.Skip(err)
				}
			}
			if err := Install(dir); err == nil {
				t.Fatal("accepted symlink")
			}
			if kind == "page" {
				data, _ := os.ReadFile(filepath.Join(outside, "target"))
				if string(data) != "preserve" {
					t.Fatal("modified link target")
				}
			}
		})
	}
}
