package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
)

// F6-05: the receiver's "no room" answer is 507, not retryable, and says how much was needed.
func TestInsufficientStorageIsAnUnretryable507WithTheShortage(t *testing.T) {
	err := fmt.Errorf("create: %w", &filetransfer.Error{Code: filetransfer.InsufficientStorage, NeededBytes: 20 << 20, FreeBytes: 4 << 20})
	recorder := httptest.NewRecorder()
	writeFileTransferError(t.Context(), "command", recorder, "request_1", err)
	var body struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
		Details   struct {
			Needed uint64 `json:"needed_bytes"`
			Free   uint64 `json:"free_bytes"`
		} `json:"details"`
	}
	if recorder.Code != http.StatusInsufficientStorage || json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
	}
	if body.Code != "insufficient_storage" || body.Retryable || body.Details.Needed != 20<<20 || body.Details.Free != 4<<20 {
		t.Fatalf("body = %+v", body)
	}
	if details := insufficientStorageDetails(&filetransfer.Error{Code: filetransfer.StorageUnavailable}); details != nil {
		t.Fatalf("an ordinary storage failure carried a shortage: %v", details)
	}
}

// X-37: the receiver's "file too large for this file system" answer is 413, not retryable, with the limit and the file system.
func TestFileTooLargeIsAnUnretryable413WithTheLimit(t *testing.T) {
	err := fmt.Errorf("create: %w", &filetransfer.Error{Code: filetransfer.FileTooLarge, FileBytes: 4500000000, FileLimit: 1<<32 - 1, FileSystem: "FAT"})
	recorder := httptest.NewRecorder()
	writeFileTransferError(t.Context(), "command", recorder, "request_1", err)
	var body struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
		Details   struct {
			File       uint64 `json:"file_bytes"`
			Limit      uint64 `json:"file_limit_bytes"`
			FileSystem string `json:"file_system"`
		} `json:"details"`
	}
	if recorder.Code != http.StatusRequestEntityTooLarge || json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
	}
	if body.Code != "file_too_large" || body.Retryable || body.Details.File != 4500000000 || body.Details.Limit != 1<<32-1 || body.Details.FileSystem != "FAT" {
		t.Fatalf("body = %+v", body)
	}
}
