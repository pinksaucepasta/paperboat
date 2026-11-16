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

func TestSendTakesAFolderAndRefusesALinkNamingThePathAndWhatToDo(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "tree")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, err := filetransfer.Prepare([]string{folder, target}, automaticTransferLimits)
	if err != nil || batch.Sources[0].Folder == nil || batch.Sources[1].Folder != nil {
		t.Fatalf("a folder and a file are both sent: %+v, %v", batch, err)
	}
	batch.Close()
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symbolic links here: ", err)
	}
	_, err = filetransfer.Prepare([]string{link}, automaticTransferLimits)
	got := missingSendSource(err, []string{link})
	if !errors.Is(got, errUsage) || !strings.Contains(got.Error(), link+" is a symbolic link") || strings.Contains(got.Error(), "could not finish") {
		t.Fatalf("%s: %v", link, got)
	}
}
