//go:build unix

package tlscert

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO or a symlink planted in the certificate directory is skipped, never opened: opening a FIFO blocks forever.
func TestLoadSkipsFifosAndSymlinksWithoutOpeningThem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certificates")
	store, err := NewDiskStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"+persistedSuffix), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link"+persistedSuffix)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		records, unsafe, err := store.Load()
		if err != nil || len(records) != 0 || unsafe != 2 {
			t.Errorf("load = %d records, unsafe=%d, err=%v", len(records), unsafe, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}
