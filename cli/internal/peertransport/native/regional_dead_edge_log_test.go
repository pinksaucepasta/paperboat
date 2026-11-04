package native_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

type moveLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *moveLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *moveLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.records = append(l.records, r.Clone())
	l.mu.Unlock()
	return nil
}
func (l *moveLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *moveLog) WithGroup(string) slog.Handler      { return l }

func captureDiagnosticLog(t *testing.T) *moveLog {
	t.Helper()
	l := &moveLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return l
}

// moveEvidence returns when a relay attempt failed with a typed reason and the
// recorded move path.
func (l *moveLog) moveEvidence(t *testing.T) (started, failed, connected time.Time, path string) {
	t.Helper()
	flush, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = diagnosticlog.Flush(flush)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.records {
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		if r.Message != "terminal carrier move" {
			continue
		}
		if attrs["stage"] == "relay_attempt" && attrs["failed"] == "true" && attrs["error_type"] != "" && failed.IsZero() {
			failed = r.Time
		}
		if attrs["stage"] == "relay_attempt_start" && started.IsZero() {
			started = r.Time
		}
		if attrs["stage"] == "connected" {
			connected, path = r.Time, attrs["path"]
		}
	}
	return started, failed, connected, path
}
