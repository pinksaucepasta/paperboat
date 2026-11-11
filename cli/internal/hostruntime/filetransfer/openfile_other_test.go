//go:build !windows

package filetransfer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A .part replaced by a symlink after Create must not redirect the upload's bytes outside Root.
func TestAppendRefusesASymlinkedPartialFile(t *testing.T) {
	service, _, root := newService(t)
	data := []byte("abcdefgh")
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(root, id+".part")
	if err := os.Remove(partial); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, partial); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Append(context.Background(), id, 0, bytes.NewReader(data)); err == nil {
		t.Fatal("Append wrote through a symlinked partial file")
	}
	if body, _ := os.ReadFile(outside); string(body) != "keep" {
		t.Fatalf("the symlink target was written: %q", body)
	}
}
