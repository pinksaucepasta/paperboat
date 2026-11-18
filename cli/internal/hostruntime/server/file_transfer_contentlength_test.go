package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/filetransfer"
)

func createSizedTransfer(t *testing.T, h *FileTransferHandler, size int64, body []byte) string {
	t.Helper()
	created, err := h.config.Service.Create(t.Context(), filetransfer.CreateRequest{
		BatchID: "fb_len", SourceMachineID: "machine_client", DestinationMachineID: "machine_host",
		InitiatingUserID: "user_1", SessionID: "ses_1",
		Files: []filetransfer.File{{Basename: "data.bin", Size: size, SHA256: transferDigest(body[:size])}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return created[0].ID
}

func committedOf(t *testing.T, h *FileTransferHandler, id string) int64 {
	t.Helper()
	got, err := h.config.Service.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.CommittedOffset
}

func TestPatchRejectsOversizeDeclaredLengthWithoutWriting(t *testing.T) {
	handler, _ := fileTransferTestHandler(t)
	data := []byte("abcdefgh")
	id := createSizedTransfer(t, handler, int64(len(data)), data)

	// Declared 1 byte past the file size, body itself is short: must fail before the spool is touched.
	req := transferRequest(http.MethodPatch, "http://helper.test/v1/file-transfers/"+id+"/content", data[:4])
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set(HeaderUploadOffset, "0")
	req.ContentLength = int64(len(data)) + 1
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "invalid_size") {
		t.Fatalf("oversize declared: status=%d body=%s", resp.Code, resp.Body.String())
	}
	if off := committedOf(t, handler, id); off != 0 {
		t.Fatalf("committed offset = %d, want 0 (nothing written)", off)
	}

	// Declared length past the 64 MiB segment cap with a tiny body: same 400, nothing written.
	req = transferRequest(http.MethodPatch, "http://helper.test/v1/file-transfers/"+id+"/content", data[:1])
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set(HeaderUploadOffset, "0")
	req.ContentLength = int64(64<<20) + 1
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "invalid_size") {
		t.Fatalf("oversize segment declared: status=%d body=%s", resp.Code, resp.Body.String())
	}
	if off := committedOf(t, handler, id); off != 0 {
		t.Fatalf("committed offset = %d, want 0", off)
	}
}

func TestPatchChunkedBodyOverFileSizeKeepsCommittedOffset(t *testing.T) {
	handler, _ := fileTransferTestHandler(t)
	data := []byte("abcdefgh")
	id := createSizedTransfer(t, handler, int64(len(data)), data)
	// Unknown length (chunked): one byte too many on the wire.
	over := append(append([]byte(nil), data...), 'x')
	req := transferRequest(http.MethodPatch, "http://helper.test/v1/file-transfers/"+id+"/content", over)
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set(HeaderUploadOffset, "0")
	req.ContentLength = -1 // chunked/unknown
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "invalid_size") {
		t.Fatalf("chunked overrun: status=%d body=%s", resp.Code, resp.Body.String())
	}
	if off := committedOf(t, handler, id); off != 0 {
		t.Fatalf("committed offset = %d, want 0", off)
	}
}
