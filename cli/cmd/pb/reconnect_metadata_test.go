package main

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// The renewal event says whether the answer was a delta or the full configuration; the diagnostics layer kept
// "stage", "failed" and "duration_ms" but dropped "delta" until it was allowed in both places.
func TestRenewalEventKeepsTheDeltaField(t *testing.T) {
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "native packet lifecycle", reflect.ValueOf((*tailnet.Authority).Refresh).Pointer())
	record.AddAttrs(slog.String("stage", "refresh_answered"), slog.Bool("failed", false), slog.Bool("delta", true), slog.Int64("duration_ms", 12))
	fields := map[string]string{}
	if !retainReconnectMetadata(record, fields) || fields["delta"] != "true" || fields["stage"] != "refresh_answered" {
		t.Fatalf("retained fields: %v", fields)
	}
	if _, err := diagnostics.NewEvent(time.Now(), "terminal", "reconnect_transport", "info", fields); err != nil {
		t.Fatalf("event with delta field refused: %v", err)
	}
}
