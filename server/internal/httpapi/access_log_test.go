package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 53 of 56 production 503s on 2026-11-05 (51 runtime-machine-services/authorize, a lease renew, a
// peer-network/config) were requests whose client had hung up: the handler's database call failed with
// context.Canceled and fell to its 503 default. Such an answer is logged 499 (client closed request);
// a 503 the server chose while the client still waited stays 503.
func TestAccessLogRecordsAHungUpClientAs499(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := accessLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusServiceUnavailable, "machine_services_unavailable", "Machine services are temporarily unavailable.", r.Context().Err())
	}))
	for name, want := range map[string]string{"client hung up": "status=499", "client waiting": "status=503"} {
		logs.Reset()
		ctx, cancel := context.WithCancel(context.Background())
		if name == "client hung up" {
			cancel()
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/runtime-machine-services/authorize", nil).WithContext(ctx))
		cancel()
		if !strings.Contains(logs.String(), want) || "status="+strconv.Itoa(w.Code) != want {
			t.Fatalf("%s: response %d, log %q; want %s", name, w.Code, logs.String(), want)
		}
	}
}

func TestAccessLogRecordsRequestAndResponseBytes(t *testing.T) {
	var logs bytes.Buffer
	handler := accessLog(slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
		_, _ = w.Write([]byte("abc"))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/machines", strings.NewReader("12345")))
	if !strings.Contains(logs.String(), "request_bytes=5") || !strings.Contains(logs.String(), "response_bytes=13") {
		t.Fatalf("log %q lacks request_bytes=5 response_bytes=13", logs.String())
	}
}
