//go:build windows

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// X-37: an Inbox on a write-protected NTFS volume (chmod R:\Inbox: The media is write protected, seen on victus) is a
// 507 the sender does not retry, with the operating system's words.
func TestWriteProtectedWindowsVolumeIsAPermanentInboxRefusal(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeFileTransferError(t.Context(), "command", recorder, "request_1", &os.PathError{Op: "chmod", Path: `R:\Inbox`, Err: windows.ERROR_WRITE_PROTECT})
	if recorder.Code != http.StatusInsufficientStorage || !strings.Contains(recorder.Body.String(), `"retryable":false`) || !strings.Contains(recorder.Body.String(), "write protected") {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
	}
}
