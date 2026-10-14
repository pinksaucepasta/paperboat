//go:build !windows

package supportbundle

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteRetainsActualPublishAndCleanupFailuresThenRecovers(t *testing.T) {
	directory := realTempDir(t)
	output := filepath.Join(directory, "bundle")
	b := mustBuilder(t)
	p, err := b.Preview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var staging string
	b.beforePublish = func(path string) error {
		staging = path
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		return syscall.EIO
	}
	_, err = b.Write(t.Context(), p, output)
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, syscall.EISDIR) {
		t.Fatal("lost actual cleanup cause")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published failed output")
	}
	if err := os.Remove(staging); err != nil {
		t.Fatal(err)
	}
	b.beforePublish = nil
	if _, err := b.Write(t.Context(), p, output); err != nil {
		t.Fatal(err)
	}
	assertNoTemporaryFiles(t, directory)
}
