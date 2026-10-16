package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
)

// A real net/http request body holds its read lock while waiting on the sender.
// Cancellation must interrupt that read, rather than wait for Body.Close to drain it.
func TestFileTransferCancelInterruptsStalledHTTPBody(t *testing.T) {
	handler, _ := fileTransferTestHandler(t)
	created, err := handler.config.Service.Create(t.Context(), filetransfer.CreateRequest{
		BatchID: "cancel_stalled_wire", SourceMachineID: "machine_client", DestinationMachineID: "machine_host",
		InitiatingUserID: "user_1", SessionID: "ses_1", Files: []filetransfer.File{{Basename: "fixture", Size: 1, SHA256: transferDigest([]byte("x"))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	endpoint := httptest.NewServer(handler)
	t.Cleanup(endpoint.Close)
	sender, err := net.DialTimeout("tcp", endpoint.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	// Send valid headers but leave the declared body stalled on the wire.
	_, err = fmt.Fprintf(sender, "PATCH /v1/file-transfers/%s/content HTTP/1.1\r\nHost: example.test\r\nAuthorization: Bearer token\r\nContent-Type: application/offset+octet-stream\r\nUpload-Offset: 0\r\n%s: sha256=%s\r\nContent-Length: 1\r\n\r\n", id, HeaderUploadDigest, transferDigest([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for handler.config.Service.ActiveCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if handler.config.Service.ActiveCount() != 1 {
		t.Fatal("wire upload did not reach its active body read")
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, endpoint.URL+"/v1/file-transfers/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer token")
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("cancel could not interrupt the stalled sender: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel status=%d", response.StatusCode)
	}
	current, err := handler.config.Service.Get(context.Background(), id)
	if err != nil || current.State != "canceled" || current.CommittedOffset != 0 || handler.config.Service.ActiveCount() != 0 {
		t.Fatalf("cancel left state=%s offset=%d active=%d err=%v", current.State, current.CommittedOffset, handler.config.Service.ActiveCount(), err)
	}
}
