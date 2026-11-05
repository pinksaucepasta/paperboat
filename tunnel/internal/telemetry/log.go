package telemetry

import (
	"context"
	"log/slog"
	"os"
	"time"
)

var eventLog = slog.New(slog.NewJSONHandler(os.Stderr, nil))

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
		"component", "paperboat-tunnel", "operation", name, "name", name, "code", name, "outcome", string(outcome)}, attrs...)
	eventLog.Log(ctx, level, name, args...)
}
