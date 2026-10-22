package inbox

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file sent to this computer lands in the Inbox as a delivered file would: whole, owner-only,
// listed, and never over an existing file.
func TestDeliverCopiesWholeFilesWithoutReplacingExistingOnes(t *testing.T) {
	root := ownerOnlyDir(t)
	content := []byte("report body")
	digest := sha256.Sum256(content)
	first, err := Deliver(root, "report.txt", int64(len(content)), digest, bytes.NewReader(content))
	if err != nil || first != filepath.Join(root, "report.txt") {
		t.Fatalf("first=%q err=%v", first, err)
	}
	second, err := Deliver(root, "report.txt", int64(len(content)), digest, bytes.NewReader(content))
	if err != nil || second != filepath.Join(root, "report (2).txt") {
		t.Fatalf("second=%q err=%v", second, err)
	}
	for _, path := range []string{first, second} {
		requirePrivateFile(t, path)
		if got, _ := os.ReadFile(path); !bytes.Equal(got, content) {
			t.Fatalf("%s holds %q", path, got)
		}
	}
	listed, err := Files(root)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listing=%v err=%v", listed, err)
	}
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".paperboat-delivery-") {
			t.Fatalf("staging file left behind: %s", entry.Name())
		}
	}
}

func TestDeliverRefusesWrongBytesAndUnsafeNames(t *testing.T) {
	root := ownerOnlyDir(t)
	content := []byte("abc")
	digest := sha256.Sum256(content)
	if _, err := Deliver(root, "a.txt", 3, digest, strings.NewReader("abd")); !errors.Is(err, ErrDeliveredDigest) {
		t.Fatalf("wrong bytes: %v", err)
	}
	if _, err := Deliver(root, "a.txt", 4, digest, bytes.NewReader(content)); !errors.Is(err, ErrDeliveredDigest) {
		t.Fatalf("wrong size: %v", err)
	}
	for _, name := range []string{"", ".hidden", "../escape", "dir/file", `win\file`} {
		if _, err := Deliver(root, name, 3, digest, bytes.NewReader(content)); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("a refused delivery left files: %v", entries)
	}
}

func ownerOnlyDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Inbox")
	if err := EnsurePath(root); err != nil {
		t.Fatal(err)
	}
	return root
}
