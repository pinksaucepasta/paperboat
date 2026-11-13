package edgehttp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestProxyRequestBodyCloseInterruptsActualSlowHTTPUpload(t *testing.T) {
	completed := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, "healthy")
			return
		}
		body := &proxyRequestBody{body: r.Body, controller: http.NewResponseController(w)}
		var first [1]byte
		if _, err := io.ReadFull(body, first[:]); err != nil {
			completed <- err
			return
		}
		readDone := make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, body); readDone <- err }()
		closeErr := body.Close()
		readErr := <-readDone
		if closeErr != nil && !expectedPrivateStreamClose(closeErr) {
			completed <- closeErr
			return
		}
		// The original native timeout remains available through the owned marker.
		if readErr != nil && !expectedPrivateStreamClose(readErr) && !errors.Is(readErr, http.ErrBodyReadAfterClose) {
			completed <- readErr
			return
		}
		completed <- nil
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 16\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !response.Close {
		t.Fatal("incomplete HTTP framing incorrectly reused connection")
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatal("slow upload did not complete")
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("cleanup cause %T", err)
		}
	case <-time.After(time.Second):
		t.Fatal("incoming upload worker leaked")
	}
	// Incomplete framing closes that connection; a fresh request must retain its
	// normal deadline and succeed rather than inheriting the expired read budget.
	healthy, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Body.Close()
	payload, err := io.ReadAll(healthy.Body)
	if err != nil || string(payload) != "healthy" {
		t.Fatal("fresh HTTP request did not recover")
	}
}

func TestPrivateStreamCloseNeverMasksJoinedOperationalCause(t *testing.T) {
	var calls atomic.Int32
	body := &dataCarrierPreviewResponseBody{body: closeOnlyReader{closeErr: errors.Join(io.EOF, syscall.EIO)}, closeRequestBody: func() error { calls.Add(1); return nil }, writeDone: readyWriteResult(errors.Join(io.EOF, context.DeadlineExceeded))}
	err := body.Close()
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatal("close lost operational sibling")
	}
	if !errors.Is(body.Close(), syscall.EIO) || calls.Load() != 1 {
		t.Fatal("repeated close changed cause")
	}
}

type closeOnlyReader struct{ closeErr error }

func (closeOnlyReader) Read([]byte) (int, error) { return 0, io.EOF }
func (r closeOnlyReader) Close() error           { return r.closeErr }
func readyWriteResult(err error) <-chan error    { done := make(chan error, 1); done <- err; return done }

func TestPreviewResponseCloseInterruptsIncompleteResponseBody(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	sent := make(chan error, 1)
	go func() { _, err := io.WriteString(remote, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n"); sent <- err }()
	response, err := http.ReadResponse(bufio.NewReader(local), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	body := &dataCarrierPreviewResponseBody{body: response.Body, stream: local}
	done := make(chan error, 1)
	go func() { done <- body.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cleanup failed: %T", err)
		}
	case <-time.After(time.Second):
		local.Close()
		t.Fatal("response Close drained a stalled remote body before closing carrier")
	}
}

func TestRequestUploadPreservesActualRequestWriteBodyCause(t *testing.T) {
	cause := &privateUploadCause{}
	upload := &requestUploadBody{body: uploadFailureReader{cause: cause}, closeBody: func() error { return nil }}
	request, err := http.NewRequest(http.MethodPost, "https://owned.example.test/", upload)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = 1
	actual := upload.preserve(request.Write(io.Discard))
	if !errors.Is(actual, cause) || cause.called.Load() != 0 {
		t.Fatal("Request.Write lost or formatted private body cause")
	}
}

type privateUploadCause struct{ called atomic.Int32 }

func (e *privateUploadCause) Error() string { e.called.Add(1); return "PRIVATE-upload-payload" }

type uploadFailureReader struct{ cause error }

func (r uploadFailureReader) Read([]byte) (int, error) { return 0, r.cause }
func (uploadFailureReader) Close() error               { return nil }
