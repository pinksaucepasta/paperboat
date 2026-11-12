//go:build windows

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// A cache file this account wrote is read; one owned by nobody we trust is not (audit C #29). The untrusted owner needs
// SYSTEM or a foreign account, so that half is shown with a path that does not exist (no owner can be read).
func TestControlAddressCacheTrustsOnlyItsOwnAccountOnWindows(t *testing.T) {
	root := t.TempDir()
	d := newControlDialer(root)
	d.cache = filepath.Join(root, "control-addresses.json")
	d.remember("api.example.test", []string{"127.0.0.1"})
	if got := d.remembered("api.example.test"); len(got) != 1 {
		t.Fatalf("remembered = %v", got)
	}
	info, err := os.Lstat(d.cache)
	if err != nil {
		t.Fatal(err)
	}
	if !cacheFileTrusted(d.cache, info) {
		t.Fatal("a cache file written by this account is not trusted")
	}
	if cacheFileTrusted(filepath.Join(root, "absent.json"), info) {
		t.Fatal("a file whose owner cannot be read was trusted")
	}
}
