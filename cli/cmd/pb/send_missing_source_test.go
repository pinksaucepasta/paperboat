package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
)

func TestSendNamesAMissingSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-file.bin")
	_, err := filetransfer.Prepare([]string{missing}, automaticTransferLimits)
	got := missingSendSource(err, []string{missing})
	if !errors.Is(got, errUsage) || !strings.Contains(got.Error(), missing+" does not exist") {
		t.Fatalf("missing source error = %v, want a refusal naming %s", got, missing)
	}
	if !errors.Is(got, os.ErrNotExist) {
		t.Fatalf("the original error is lost: %v", got)
	}
	internal := &os.PathError{Op: "open", Path: "/state/inbox.json", Err: os.ErrNotExist}
	if got := missingSendSource(internal, []string{missing}); got != error(internal) {
		t.Fatalf("a path the user did not give was rewritten: %v", got)
	}
}

func TestSendRefusesAFolderAndALinkNamingThePathAndWhatToDo(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "tree")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symbolic links here: ", err)
	}
	for path, want := range map[string]string{folder: "is a folder", link: "is a symbolic link"} {
		_, err := filetransfer.Prepare([]string{path}, automaticTransferLimits)
		got := missingSendSource(err, []string{path})
		if !errors.Is(got, errUsage) || !strings.Contains(got.Error(), path+" "+want) || strings.Contains(got.Error(), "could not finish") {
			t.Fatalf("%s: %v", path, got)
		}
	}
	if _, err := filetransfer.Prepare([]string{target}, automaticTransferLimits); err != nil {
		t.Fatalf("a regular file is still sent: %v", err)
	}
}
