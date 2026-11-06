package controlplane

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

// 2026-11-06 00:10:00Z: the same hang-up on an edge control request was reported as control_unavailable.
func TestEdgeFailureOfAHungUpEdgeIsNotReportedAsAnOutage(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for name, hungUp := range map[string]bool{"edge hung up": true, "edge waiting": false} {
		logs.Reset()
		ctx, cancel := context.WithCancel(context.Background())
		if hungUp {
			cancel()
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/edge/routes/desired-state", nil).WithContext(ctx)
		writeEdgeError(httptest.NewRecorder(), r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, errors.New("database unavailable"))
		cancel()
		if reported := strings.Contains(logs.String(), "operation failed"); reported == hungUp {
			t.Fatalf("%s: reported=%v, log %q", name, reported, logs.String())
		}
	}
}
