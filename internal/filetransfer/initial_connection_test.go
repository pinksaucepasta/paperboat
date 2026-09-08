package filetransfer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNativeInitialConnectionDoesNotConsumeRetryWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"batch_id":"fb_connect","transfers":[]}`))
	}))
	defer server.Close()
	stop := make(chan struct{})
	defer close(stop)
	client, err := NewNativeClient("https://machine.example/v1/file-transfers", Auth{Token: "token"}, testBinding(), func(ctx context.Context) (net.Conn, error) {
		timer := time.NewTimer(operationRecoveryWindow + 100*time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-stop:
			return nil, context.Canceled
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), operationRecoveryWindow+3*time.Second)
	defer cancel()
	var batch Batch
	if err := client.retryJSONRequest(ctx, http.MethodPost, client.Endpoint, "op_connect", "application/json", 0, []byte(`{"batch_id":"fb_connect"}`), &batch); err != nil {
		t.Fatalf("initial native connection was cut off by retry budget: %v", err)
	}
	if batch.BatchID != "fb_connect" {
		t.Fatalf("batch=%q", batch.BatchID)
	}
}

func TestJSONRecoveryDeadlineStartsAtFailureAndDoesNotReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	initialDeadline, _ := ctx.Deadline()
	var retryDeadline time.Time
	calls := 0
	client := NewClient("https://machine.example/v1/file-transfers", Auth{Token: "token"}, testBinding(), &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("request has no deadline")
		}
		if calls == 1 {
			if !deadline.Equal(initialDeadline) {
				t.Errorf("initial request replaced caller deadline")
			}
		} else if calls == 2 {
			retryDeadline = deadline
			if left := time.Until(deadline); left <= 0 || left > operationRecoveryWindow {
				t.Errorf("retry budget=%s", left)
			}
		} else if !deadline.Equal(retryDeadline) {
			t.Error("subsequent failure extended retry deadline")
		}
		return nil, errors.New("transport unavailable")
	})})
	client.retryWait = func(context.Context, int) error {
		if calls >= 3 {
			return context.DeadlineExceeded
		}
		return nil
	}
	if err := client.retryJSONRequest(ctx, http.MethodPost, client.Endpoint, "op_retry", "application/json", 0, []byte(`{}`), &Batch{}); err == nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
