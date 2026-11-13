package filetransfer

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func noHardLinks(t *testing.T) {
	t.Helper()
	old := linkFile
	linkFile = func(from, to string) error { return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EPERM} }
	t.Cleanup(func() { linkFile = old })
}

func TestPublishOnAFileSystemWithoutHardLinksRenamesTheFinishedCopy(t *testing.T) {
	noHardLinks(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "content")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(inbox, "file.bin")
	if err := (&Service{}).copyToInbox(src, inbox, path, "tx"); err != nil {
		t.Fatalf("publish failed instead of renaming the copy: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "payload" {
		t.Fatalf("inbox file = %q, %v", got, err)
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 1 {
		t.Fatalf("leftovers in the Inbox: %v", entries)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the spool content stayed after publishing: %v", err)
	}
}

func TestPublishAcrossVolumesCopiesThenRemovesTheSpoolContent(t *testing.T) {
	old := moveFile
	moveFile = func(from, to string) error {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { moveFile = old })
	dir := t.TempDir()
	src := filepath.Join(dir, "content")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(inbox, "file.bin")
	if err := (&Service{}).copyToInbox(src, inbox, path, "tx"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "payload" {
		t.Fatalf("inbox file = %q, %v", got, err)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the spool content stayed after the copy: %v", err)
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 1 {
		t.Fatalf("leftovers in the Inbox: %v", entries)
	}
}

func TestPublishThatFindsAForeignFileKeepsTheSpoolContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "content")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(inbox, "file.bin")
	if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Service{}).copyToInbox(src, inbox, path, "tx"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != "new" {
		t.Fatalf("the spool content was lost on a failed publish: %q %v", got, err)
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 1 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
}

func TestPublishWithoutHardLinksNeverReplacesAnExistingFile(t *testing.T) {
	noHardLinks(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "content")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(inbox, "file.bin")
	if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (&Service{}).copyToInbox(src, inbox, path, "tx")
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "theirs" {
		t.Fatalf("existing file was replaced: %q", got)
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 1 {
		t.Fatalf("temporary copy left behind: %v", entries)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != "new" {
		t.Fatalf("the spool content was lost on a failed publish: %q %v", got, err)
	}
}
