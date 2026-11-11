//go:build darwin || linux

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// The remembered-address file is written atomically and owner-only, and a link at its path is neither written
// through nor read.
func TestControlAddressCacheRefusesALink(t *testing.T) {
	root := t.TempDir()
	d := newControlDialer(root)
	d.cache = filepath.Join(root, "control-addresses.json")
	d.remember("api.example.test", []string{"127.0.0.1"})
	if info, err := os.Lstat(d.cache); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file %v %v, want 0600", info, err)
	}
	if got := d.remembered("api.example.test"); len(got) != 1 {
		t.Fatalf("remembered = %v", got)
	}
	target := filepath.Join(root, "elsewhere.json")
	if err := os.Rename(d.cache, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, d.cache); err != nil {
		t.Fatal(err)
	}
	if got := d.remembered("api.example.test"); got != nil {
		t.Fatalf("a linked cache was read: %v", got)
	}
	before, _ := os.ReadFile(target)
	d.remember("api.example.test", []string{"127.0.0.2"})
	if after, _ := os.ReadFile(target); string(after) != string(before) {
		t.Fatal("remember wrote through a link")
	}
}

// A cache file other accounts can write or read is not trusted (audit C #29): it is ignored, never used as addresses.
func TestControlAddressCacheRefusesAFileOthersCanWrite(t *testing.T) {
	root := t.TempDir()
	d := newControlDialer(root)
	d.cache = filepath.Join(root, "control-addresses.json")
	d.remember("api.example.test", []string{"127.0.0.1"})
	if got := d.remembered("api.example.test"); len(got) != 1 {
		t.Fatalf("remembered = %v", got)
	}
	if err := os.Chmod(d.cache, 0o666); err != nil {
		t.Fatal(err)
	}
	if got := d.remembered("api.example.test"); got != nil {
		t.Fatalf("a world-writable cache was read: %v", got)
	}
}
