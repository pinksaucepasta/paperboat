package diskspace

import (
	"path/filepath"
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
