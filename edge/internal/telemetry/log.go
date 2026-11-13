package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"
)

var eventLog = slog.New(slog.NewJSONHandler(os.Stderr, nil))

// CaptureLog sends event lines to w instead of stderr and returns the restore function. For tests that assert on what
// the edge logged; not safe to call concurrently with Log.
func CaptureLog(w io.Writer) (restore func()) {
	previous := eventLog
	eventLog = slog.New(slog.NewJSONHandler(w, nil))
	return func() { eventLog = previous }
}

// Log writes one edge event line to stderr in the edge_event schema, the form of every line the
// edge process prints (route_lifecycle uses the same fields). Callers pass only fixed codes and
// bounded values in attrs.
func Log(ctx context.Context, severity EventSeverity, name string, outcome EventOutcome, attrs ...any) {
	level := slog.LevelInfo
	switch severity {
	case SeverityWarn:
		level = slog.LevelWarn
	case SeverityError:
		level = slog.LevelError
	case SeverityDebug:
		level = slog.LevelDebug
	}
	args := append([]any{"schema", EventSchemaV1, "at", time.Now().UTC(), "severity", string(severity),
		"component", "paperboat-edge", "operation", name, "name", name, "code", name, "outcome", string(outcome)}, attrs...)
	eventLog.Log(ctx, level, name, args...)
}
