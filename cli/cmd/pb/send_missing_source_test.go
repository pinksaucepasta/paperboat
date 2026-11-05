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
