package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/coder/websocket"
)

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) { return 0, syscall.EIO }

type shortOutput struct{}

func (shortOutput) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestExchangeRetainsFailureAndRecovers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		kind, data, err := connection.Read(r.Context())
		if err == nil {
			_ = connection.Write(r.Context(), kind, data)
		}
	}))
	defer server.Close()
	address := "ws" + strings.TrimPrefix(server.URL, "http")
	const payload = "PRIVATE_PREVIEW_PAYLOAD"
	err := exchange(t.Context(), address, payload, failedOutput{})
	if !errors.Is(err, syscall.EIO) || strings.Contains(err.Error(), payload) || strings.Contains(err.Error(), address) {
		t.Fatalf("output failure lost its cause or leaked exchange data: %v", err)
	}
	if err := exchange(t.Context(), address, payload, shortOutput{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("truncated response was not reported", err)
	}
	var output bytes.Buffer
	if err := exchange(t.Context(), address, payload, &output); err != nil || output.String() != payload {
		t.Fatal("fresh exchange did not recover", err)
	}
	err = exchange(context.Background(), "://PRIVATE_PREVIEW_URL", payload, &output)
	var native *url.Error
	if !errors.As(err, &native) || strings.Contains(err.Error(), "PRIVATE_PREVIEW_URL") {
		t.Fatal("invalid URL lost its cause or leaked the URL", err)
	}
}
