package filetransfer

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// F6-05: a receiver with no room says so with the shortage, and the sender does not retry it.
func TestInsufficientStorageIsPermanentAndCarriesTheShortage(t *testing.T) {
	body := `{"code":"insufficient_storage","requestId":"request_1","retryable":false,"details":{"needed_bytes":20971520,"free_bytes":4194304}}`
	err := decodeError(&http.Response{StatusCode: http.StatusInsufficientStorage, Body: io.NopCloser(strings.NewReader(body))})
	remote, ok := err.(*Error)
	if !ok || remote.Code != "insufficient_storage" || remote.NeededBytes != 20<<20 || remote.FreeBytes != 4<<20 {
		t.Fatalf("decoded = %#v", err)
	}
	if retryableTransferFailure(err) {
		t.Fatal("a full disk would be retried until the timeout")
	}
	unavailable := decodeError(&http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{"code":"storage_unavailable","retryable":true}`))})
	if !retryableTransferFailure(unavailable) {
		t.Fatal("an ordinary storage hiccup is no longer retried")
	}
}

// X-37: a receiver whose file system cannot hold the file says so with the limit, and the sender does not retry it.
func TestFileTooLargeIsPermanentAndCarriesTheLimit(t *testing.T) {
	body := `{"code":"file_too_large","requestId":"request_1","retryable":false,"details":{"file_bytes":4500000000,"file_limit_bytes":4294967295,"file_system":"FAT"}}`
	err := decodeError(&http.Response{StatusCode: http.StatusRequestEntityTooLarge, Body: io.NopCloser(strings.NewReader(body))})
	remote, ok := err.(*Error)
	if !ok || remote.Code != "file_too_large" || remote.FileBytes != 4500000000 || remote.FileLimit != 1<<32-1 || remote.FileSystem != "FAT" {
		t.Fatalf("decoded = %#v", err)
	}
	if retryableTransferFailure(err) {
		t.Fatal("a file the file system cannot hold would be retried until the timeout")
	}
}
