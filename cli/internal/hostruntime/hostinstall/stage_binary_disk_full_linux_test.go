//go:build linux

package hostinstall

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// F14-03 (disk full): staging the new executable on a really full filesystem (kernel ENOSPC on a tiny
// tmpfs, no write seam) is the typed insufficient-space refusal. The previously installed binary in
// that directory is byte-for-byte untouched and the partial staging file is removed.
func TestStageBinaryOnAFullDiskIsRefusedAndKeepsThePreviousBinary(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mounting a small filesystem and staging as root-owned state requires root")
	}
	request := validRequest(t)
	mount := t.TempDir()
	if err := syscall.Mount("tmpfs", mount, "tmpfs", 0, "size=64k"); err != nil {
		t.Skipf("cannot mount a tmpfs here: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Unmount(mount, 0); err != nil {
			t.Errorf("unmounting the small filesystem: %v", err)
		}
	})
	directory := filepath.Join(mount, "releases")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := bytes.Repeat([]byte("previous"), 4)
	destination := filepath.Join(directory, "pb")
	if err := os.WriteFile(destination, previous, 0o755); err != nil {
		t.Fatal(err)
	}
	ballast, err := os.Create(filepath.Join(mount, "ballast"))
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{1}, 4096)
	for {
		if _, err := ballast.Write(chunk); err != nil {
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("filling the filesystem: %v", err)
			}
			break
		}
	}
	if err := ballast.Close(); err != nil {
		t.Fatal(err)
	}

	err = stageBinary(request.Executable, destination, request.Source)
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("stageBinary on a full disk = %v, want ErrInsufficientSpace", err)
	}
	if got, readErr := os.ReadFile(destination); readErr != nil || !bytes.Equal(got, previous) {
		t.Fatalf("the previous binary changed after a refused staging: %v", readErr)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".binary-") {
			t.Fatalf("partial staging file %s left behind", entry.Name())
		}
	}
}
