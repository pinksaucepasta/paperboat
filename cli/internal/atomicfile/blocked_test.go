package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestAbsentOrBlockedTellsAFileInThePathFromAbsence(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(blocker, "state.json"), filepath.Join(blocker, "state", "state.json")} {
		_, statErr := os.Lstat(path)
		err := AbsentOrBlocked(path, statErr)
		var pathErr *fs.PathError
		if !errors.Is(err, ErrNotDirectory) || errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr) {
			t.Fatalf("%s under a file: %v", path, err)
		}
	}
	absent := filepath.Join(dir, "missing", "state.json")
	_, statErr := os.Lstat(absent)
	if err := AbsentOrBlocked(absent, statErr); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotDirectory) {
		t.Fatalf("absent state: %v", err)
	}
	if err := AbsentOrBlocked(absent, nil); err != nil {
		t.Fatalf("no error: %v", err)
	}
}
