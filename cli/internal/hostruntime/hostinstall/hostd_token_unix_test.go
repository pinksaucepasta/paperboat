//go:build darwin || linux

package hostinstall

import (
	"os"
	"path/filepath"
	"testing"
)

// F21-159: the hostd token is created by temp file + rename: complete and 0600, no temp file left behind,
// and a destination that cannot be replaced leaves no empty token (an in-place write left a 0-byte file that
// a later install kept).
func TestNewHostdTokenIsCompleteAndAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostd.token")
	if err := writeNewHostdToken(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 32 || info.Mode().Perm() != 0o600 {
		t.Fatalf("token = %v, %v; want 32 bytes, 0600", info, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := writeNewHostdToken(filepath.Join(blocked, "hostd.token")); err == nil {
			t.Fatal("write into a read-only directory succeeded")
		}
		if entries, _ := os.ReadDir(blocked); len(entries) != 0 {
			t.Fatalf("a failed write left %v", entries)
		}
	}
}
