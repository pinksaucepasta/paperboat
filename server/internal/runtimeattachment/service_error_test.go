package runtimeattachment

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 2026-11-06 00:10:05Z: a replaced edge process cancelled its in-flight call as it exited, and the
// server reported a carrier_unavailable 503 (context_canceled) for a caller that was already gone.
func TestServiceErrorOfAHungUpCallerIsNotReportedAsAnOutage(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for name, hungUp := range map[string]bool{"caller hung up": true, "caller waiting": false} {
		logs.Reset()
		ctx, cancel := context.WithCancel(context.Background())
		if hungUp {
			cancel()
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/runtime/carrier-attachment", nil).WithContext(ctx)
		serviceError(httptest.NewRecorder(), r, errors.New("database unavailable"))
		cancel()
		if reported := strings.Contains(logs.String(), "operation failed"); reported == hungUp {
			t.Fatalf("%s: reported=%v, log %q", name, reported, logs.String())
		}
	}
}
