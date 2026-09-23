//go:build linux

package deviceguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGuardUnitRejectsForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.service")
	before := []byte("[Service]\nExecStart=/bin/true\n")
	if err := os.WriteFile(path, before, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeGuardUnit(path, "replacement"); err == nil {
		t.Fatal("foreign unit overwritten")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("foreign unit changed")
	}
	link := filepath.Join(t.TempDir(), "guard.service")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err = writeGuardUnit(link, "replacement"); err == nil {
		t.Fatal("unit symlink followed")
	}
}
