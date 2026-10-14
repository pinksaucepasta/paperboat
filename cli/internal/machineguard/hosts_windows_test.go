//go:build windows

package machineguard

import (
	"bytes"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestMachineHostsLockedWindowsFilePreservesOriginal(t *testing.T) {
	path := testHostsFile(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	name, _ := windows.UTF16PtrFromString(path)
	// Reads and metadata checks can proceed, but atomic replacement is denied.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := replaceHostsFile(t.Context(), path, info, original, []byte("replacement")); err == nil {
		t.Fatal("locked file replaced")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("original damaged: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".paperboat-hosts-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("staging files leaked: %v %v", files, err)
	}
}
