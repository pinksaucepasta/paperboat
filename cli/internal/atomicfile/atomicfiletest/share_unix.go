//go:build !windows

package atomicfiletest

import (
	"os"
	"testing"
)

// MakeAccessible undoes WriteOwnerOnly: the file becomes readable by others, as a careless user or tool leaves it.
func MakeAccessible(t testing.TB, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// WriteOwnerAndSystemOnly is WriteOwnerOnly for the files whose Windows form allows exactly SYSTEM and the owner
// (no Administrators entry), such as enrollment token files; on Unix both are mode 0600.
func WriteOwnerAndSystemOnly(t testing.TB, path string, data []byte) {
	t.Helper()
	WriteOwnerOnly(t, path, data)
}
