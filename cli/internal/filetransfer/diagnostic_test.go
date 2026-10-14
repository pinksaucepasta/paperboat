package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func TestHTTPFailureProjectsStatusWithoutResponseTextOrIDs(t *testing.T) {
	err := &Error{Code: "server_private_code", Message: "private response payload", StatusCode: 429, RequestID: "request_private_identifier"}
	fault := errorreport.ProjectFault(context.Background(), "pb", "file_transfer", "command", "unexpected_cli_failure", err)
	if fault.Stage != "command" || fault.Code != "file_transfer_failed" || fault.HTTPStatus != 429 || fault.Cause != "rate_limited" {
		t.Fatalf("projected transfer fault=%+v", fault)
	}
	serialized := fmt.Sprintf("%+v", fault)
	for _, private := range []string{"private response payload", "request_private_identifier", "server_private_code"} {
		if strings.Contains(serialized, private) {
			t.Fatalf("projected transfer fault retained %q: %s", private, serialized)
		}
	}
}

func TestDeliveryTimeoutHasStaticPhaseAndPreservesDeadline(t *testing.T) {
	err := fileTransferPhaseFailure("delivery", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "file delivery timed out" {
		t.Fatalf("delivery error=%v", err)
	}
	fault := errorreport.ProjectFault(context.Background(), "pb", "file_transfer", "command", "unexpected_cli_failure", err)
	if fault.Stage != "delivery" || fault.Code != "file_transfer_failed" || fault.Cause != "deadline_exceeded" {
		t.Fatalf("projected delivery fault=%+v", fault)
	}
}

func TestDeliveryContextErrorLeavesUserCancellationUnwrapped(t *testing.T) {
	cause := errors.New("caller stopped delivery")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	err := deliveryContextError(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("cancellation status or cause lost: %v", err)
	}
}

type observedResponseBody struct {
	reader    io.Reader
	readErr   error
	closeErr  error
	closeCall int
}

func (b *observedResponseBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		err := b.readErr
		b.readErr = nil
		return 0, err
	}
	return b.reader.Read(p)
}

func (b *observedResponseBody) Close() error {
	b.closeCall++
	return b.closeErr
}

func TestClientResponseReadAndCloseFailuresRetainCauses(t *testing.T) {
	readCause := syscall.EIO
	closeCause := errors.New("private response close detail")
	for _, tc := range []struct {
		name       string
		status     int
		body       *observedResponseBody
		wantCause  error
		wantStatus int
		wantText   string
	}{
		{
			name:       "error response read and close",
			status:     http.StatusBadRequest,
			body:       &observedResponseBody{readErr: readCause, closeErr: closeCause},
			wantCause:  readCause,
			wantStatus: http.StatusBadRequest,
			wantText:   "file transfer failed: http_error (HTTP 400 Bad Request)",
		},
		{
			name:       "successful response close",
			status:     http.StatusOK,
			body:       &observedResponseBody{reader: strings.NewReader(`{"transfer_id":"ft_1","state":"stored"}`), closeErr: closeCause},
			wantCause:  closeCause,
			wantStatus: 0,
			wantText:   "file transfer failed: http_error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := NewClient("https://route.example/v1/file-transfers", Auth{Token: "token"}, testBinding(), &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: tc.body, Request: request}, nil
			})})
			client.retryWait = func(context.Context, int) error { return context.DeadlineExceeded }
			_, err := client.Status(context.Background(), "ft_1")
			var responseErr *Error
			if !errors.As(err, &responseErr) || !errors.Is(err, tc.wantCause) || responseErr.StatusCode != tc.wantStatus {
				t.Fatalf("response failure=%T %v, want cause %v and status %d", err, err, tc.wantCause, tc.wantStatus)
			}
			if calls != 1 || tc.body.closeCall != 1 || err.Error() != tc.wantText || strings.Contains(err.Error(), "private response") {
				t.Fatalf("response cleanup/error calls=%d closes=%d prose=%q", calls, tc.body.closeCall, err)
			}
			if tc.name == "error response read and close" && !errors.Is(err, closeCause) {
				t.Fatal("error response close cause was lost")
			}
		})
	}
}
