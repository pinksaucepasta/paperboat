package main

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	bugreportpkg "github.com/pinksaucepasta/paperboat/internal/bugreport"
)

func TestBugreportFailurePresentationPreservesConfirmedUpload(t *testing.T) {
	result := bugreportpkg.Result{BundleCreated: true, Uploaded: true}
	err := &bugreportpkg.StageError{Stage: "close local bundle", Result: result, Err: syscall.EIO}
	failure := bugreportJSONFailure(err, result)
	if failure.Code != "bugreport_cleanup_failed" || failure.Stage != "close local bundle" || !strings.Contains(failure.Message, "was uploaded") || !strings.Contains(failure.Message, "local copy was preserved") {
		t.Fatalf("confirmed upload was presented as failed upload: %+v", failure)
	}
	if !strings.Contains(err.Error(), "upload completed") || strings.Contains(err.Error(), "retry") || !errors.Is(err, syscall.EIO) {
		t.Fatal("confirmed upload cleanup lost its cause or advised repeating completed work")
	}
	result.Uploaded = false
	if failure := bugreportJSONFailure(err, result); failure.Code != "bugreport_upload_failed" {
		t.Fatalf("unconfirmed upload presentation changed: %+v", failure)
	}
	if failure := bugreportJSONFailure(err, bugreportpkg.Result{}); failure.Code != "bugreport_failed" {
		t.Fatalf("uncreated bundle presentation changed: %+v", failure)
	}
}
