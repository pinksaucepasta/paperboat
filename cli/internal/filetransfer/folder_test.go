package filetransfer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// makeTree builds nested folders, an empty folder, a non-ASCII name and a name Windows reserves.
func makeTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "proj")
	for _, dir := range []string{"src/deep/er", "empty"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"top.txt": "top", "src/main.go": "package main", "src/deep/er/ünï.txt": "unicode", "CON.txt": "reserved", "src/empty.bin": ""} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readAll(t *testing.T, source Source) []byte {
	t.Helper()
	if _, err := source.Reader.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(source.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func tarNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	archive := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			t.Fatalf("entry %s has type %c", header.Name, header.Typeflag)
		}
		names = append(names, header.Name)
	}
}

func TestPrepareSendsAFolderTreeWithItsEmptyFolders(t *testing.T) {
	root := makeTree(t)
	batch, err := Prepare([]string{root}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	source := batch.Sources[0]
	data := readAll(t, source)
	want := []string{"CON.txt", "empty/", "src/", "src/deep/", "src/deep/er/", "src/deep/er/ünï.txt", "src/empty.bin", "src/main.go", "top.txt"}
	if got := tarNames(t, data); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	if source.Basename != "proj" || source.Size != int64(len(data)) || source.SHA256 != sha256.Sum256(data) {
		t.Fatalf("source %q size %d does not describe the stream (%d bytes)", source.Basename, source.Size, len(data))
	}
	if source.Folder == nil || source.Folder.Files != 5 || len(source.Folder.Paths) != len(want) {
		t.Fatalf("folder summary = %+v", source.Folder)
	}
	// The same bytes every pass: resume depends on it.
	if !bytes.Equal(readAll(t, source), data) {
		t.Fatal("a second pass over the folder produced other bytes")
	}
}

func TestFolderStreamResumesFromACommittedOffsetInEitherDirection(t *testing.T) {
	batch, err := Prepare([]string{makeTree(t)}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	source := batch.Sources[0]
	whole := readAll(t, source)
	for _, offset := range []int64{0, 1, 511, 512, 1500, int64(len(whole)) - 1, int64(len(whole))} {
		if _, err := source.Reader.Seek(offset, io.SeekStart); err != nil {
			t.Fatalf("seek %d: %v", offset, err)
		}
		rest, err := io.ReadAll(source.Reader)
		if err != nil || !bytes.Equal(rest, whole[offset:]) {
			t.Fatalf("resume at %d: %d bytes, err %v, want %d", offset, len(rest), err, len(whole)-int(offset))
		}
	}
}

func TestFolderSkipsLinksAndNamesThemTheSameOnEveryOS(t *testing.T) {
	root := makeTree(t)
	if err := os.Symlink("top.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	if err := os.Symlink("src", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	batch, err := Prepare([]string{root}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	skipped := batch.Sources[0].Folder.Skipped
	var got []string
	for _, entry := range skipped {
		got = append(got, entry.Path+":"+entry.Kind)
	}
	sort.Strings(got)
	if strings.Join(got, "|") != "dirlink:symbolic link|link.txt:symbolic link" {
		t.Fatalf("skipped = %q", got)
	}
	for _, name := range tarNames(t, readAll(t, batch.Sources[0])) {
		if strings.Contains(name, "link") {
			t.Fatalf("a link was sent: %s", name)
		}
	}
	// A link given as the thing to send is still refused, not followed.
	var notRegular *NotRegularError
	if _, err := Prepare([]string{filepath.Join(root, "link.txt")}, Limits{}); !errors.As(err, &notRegular) || !notRegular.Symlink {
		t.Fatalf("a symbolic link argument = %v", err)
	}
}

func TestFolderSendsAnEmptyFolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nothing")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	batch, err := Prepare([]string{root}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	if got := tarNames(t, readAll(t, batch.Sources[0])); len(got) != 0 {
		t.Fatalf("entries = %q", got)
	}
	if batch.Sources[0].Folder.Files != 0 {
		t.Fatal("an empty folder has files")
	}
}

func TestFolderLimitsApplyToTheTotal(t *testing.T) {
	root := makeTree(t)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 3000), 0o600); err != nil {
		t.Fatal(err)
	}
	var limit *LimitError
	if _, err := Prepare([]string{root}, Limits{MaxFileBytes: 2000}); !errors.As(err, &limit) || limit.Scope != LimitFileBytes || limit.Name != "proj" || limit.Value < 3000 {
		t.Fatalf("a folder over the size limit = %v", err)
	}
	// The tar's own framing counts too: contents (3030 bytes) under the limit still exceed it as a stream.
	if _, err := Prepare([]string{root}, Limits{MaxFileBytes: 3100}); !errors.As(err, &limit) || limit.Value <= 3100 {
		t.Fatalf("the framing is not counted: %v", err)
	}
	other := filepath.Join(t.TempDir(), "one.bin")
	if err := os.WriteFile(other, make([]byte, 3000), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare([]string{makeTree(t), other}, Limits{MaxBatchBytes: 4000}); !errors.As(err, &limit) || limit.Scope != LimitBatchBytes {
		t.Fatalf("folder plus file over the batch limit = %v", err)
	}
}

func TestFolderWithTooManyEntriesIsRefusedBeforeAnythingIsSent(t *testing.T) {
	old := folderEntryLimit
	folderEntryLimit = 3
	defer func() { folderEntryLimit = old }()
	var tooMany *FolderEntriesError
	if _, err := Prepare([]string{makeTree(t)}, Limits{}); !errors.As(err, &tooMany) || tooMany.Limit != 3 {
		t.Fatalf("a folder over the entry limit = %v", err)
	}
}

func TestFolderRefusesAFileSystemRoot(t *testing.T) {
	root := string(filepath.Separator)
	if v := filepath.VolumeName(os.TempDir()); v != "" {
		root = v + root
	}
	if _, err := Prepare([]string{root}, Limits{}); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("a file system root = %v", err)
	}
}

func TestFolderChangedWhileSendingFailsInsteadOfSendingOtherBytes(t *testing.T) {
	root := makeTree(t)
	batch, err := Prepare([]string{root}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	if err := os.WriteFile(filepath.Join(root, "top.txt"), []byte("grown after the scan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := batch.Sources[0].Reader.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(batch.Sources[0].Reader); err == nil || !strings.Contains(err.Error(), "changed while sending") {
		t.Fatalf("a file that changed = %v", err)
	}
}
