package inbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, root, name, content string, modified time.Time) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestFilesListsDeliveredFilesNewestFirstAndFindsByID(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	writeFile(t, root, "old.txt", "old", base)
	writeFile(t, root, "new.txt", "newer", base.Add(time.Hour))
	writeFile(t, root, ".paperboat-receipts.json", "{}", base.Add(2*time.Hour))
	writeFile(t, root, ".paperboat-transfer-x.part", "partial", base.Add(2*time.Hour))
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "old.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	files, err := Files(root)
	if err != nil || len(files) != 2 || files[0].Name != "new.txt" || files[0].Size != 5 || files[1].Name != "old.txt" {
		t.Fatalf("files = %+v err=%v", files, err)
	}
	found, err := Find(root, FileID("old.txt"))
	if err != nil || found.Name != "old.txt" {
		t.Fatalf("find = %+v err=%v", found, err)
	}
	for _, id := range []string{FileID("link.txt"), FileID(".paperboat-receipts.json"), FileID("folder"), "", "../etc/passwd", "ABCDEF0123456789", "short"} {
		if _, err := Find(root, id); !errors.Is(err, ErrFileNotFound) {
			t.Errorf("id %q: %v", id, err)
		}
	}
}

func TestOpenReadsOnlyDeliveredRegularFiles(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	writeFile(t, root, "a.txt", "hello", base)
	handle, file, err := Open(root, FileID("a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	buffer := make([]byte, 16)
	n, _ := handle.Read(buffer)
	if string(buffer[:n]) != "hello" || file.Size != 5 || file.Name != "a.txt" {
		t.Fatalf("read %q file=%+v", buffer[:n], file)
	}
	// Replaced by a link after the listing: refused, never followed.
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, FileID("a.txt")); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("link followed: %v", err)
	}
}

func TestRemoveDeletesOneFileAndKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	writeFile(t, root, "a.txt", "a", base)
	writeFile(t, root, "b.txt", "b", base)
	writeFile(t, root, ".paperboat-receipts.json", "{}", base)
	if err := Remove(root, FileID("a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a.txt still there: %v", err)
	}
	for _, name := range []string{"b.txt", ".paperboat-receipts.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("%s removed: %v", name, err)
		}
	}
	if err := Remove(root, FileID("a.txt")); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	if err := Remove(root, FileID(".paperboat-receipts.json")); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("hidden file removable: %v", err)
	}
}
