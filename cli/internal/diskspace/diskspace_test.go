package diskspace

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAvailableMeasuresTheDiskAPathWouldBeCreatedOn(t *testing.T) {
	directory := t.TempDir()
	existing, err := Available(directory)
	if err != nil || existing == 0 {
		t.Fatalf("existing directory: %d %v", existing, err)
	}
	missing, err := Available(filepath.Join(directory, "does", "not", "exist", "yet"))
	if err != nil || missing != existing && missing == 0 {
		t.Fatalf("missing path measured through its parent: %d %v", missing, err)
	}
	if _, err := Available("relative/path"); err == nil {
		t.Fatal("a relative path was accepted")
	}
}

func TestIsFullRecognisesTheDiskFullErrnoThroughWrapping(t *testing.T) {
	for _, err := range []error{syscall.ENOSPC, syscall.EDQUOT} {
		wrapped := &os.PathError{Op: "write", Path: "x", Err: fmt.Errorf("copy: %w", err)}
		if !IsFull(wrapped) {
			t.Fatalf("%v not recognised", err)
		}
	}
	if IsFull(syscall.EACCES) || IsFull(nil) {
		t.Fatal("an unrelated error was taken for a full disk")
	}
}

func TestMaxFileIsUnlimitedOnTheTestDisk(t *testing.T) {
	limit, name, err := MaxFile(filepath.Join(t.TempDir(), "not", "yet"))
	if err != nil || limit != 0 || name == "" {
		t.Fatalf("a normal disk has no per-file limit: %d %q %v", limit, name, err)
	}
	if _, _, err := MaxFile("relative"); err == nil {
		t.Fatal("a relative path was accepted")
	}
}
