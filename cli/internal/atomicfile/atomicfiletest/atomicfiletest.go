// Package atomicfiletest checks the privacy guarantee of files written by atomicfile in tests.
// Unix files carry it in their mode bits; Windows files carry it in a protected DACL, and report
// mode 0666 whatever their ACL says, so mode assertions are meaningless there.
package atomicfiletest

import (
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

// WriteOwnerOnly creates a fixture the product will accept as private: mode 0600 on Unix, a protected owner-only
// DACL on Windows, where os.WriteFile(path, data, 0600) inherits the directory's ACL and is refused.
func WriteOwnerOnly(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := atomicfile.Write(path, data, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		t.Fatalf("write owner-only fixture: %v", err)
	}
}

// RequireOwnerOnly fails the test unless path is accessible only by its owner (plus SYSTEM and
// Administrators on Windows). mode is the exact Unix mode the writer was asked for, normally 0600.
func RequireOwnerOnly(t testing.TB, path string, mode os.FileMode) {
	t.Helper()
	if err := checkOwnerOnly(path, mode); err != nil {
		t.Fatalf("%s is not owner-only: %v", path, err)
	}
}
