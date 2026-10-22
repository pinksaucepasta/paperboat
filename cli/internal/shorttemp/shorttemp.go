// Package shorttemp creates test directories that are safe for Unix sockets and for code that
// refuses symlinked ancestors. macOS temporary directories live under /var/folders (a symlink to
// /private/var) and, with the test name appended, exceed the 104-byte socket path limit.
package shorttemp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Dir returns an empty directory with a short, symlink-free path, removed when the test ends.
func Dir(t testing.TB) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "pb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}
