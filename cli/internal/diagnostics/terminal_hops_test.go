package diagnostics

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

func TestTerminalHopCaptureKeepsRangesAndObservationTimeWithoutIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := diagnosticlog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	previous := slog.Default()
	defer slog.SetDefault(previous)
	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Setenv("PAPERBOAT_TERMINAL_HOPS", "")
	TerminalHop("pty_read", "PRIVATE_RESOURCE", 12, 23, time.UnixMicro(123456))
	if err := diagnosticlog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("capture was enabled without explicit request")
	}
	t.Setenv("PAPERBOAT_TERMINAL_HOPS", "1")
	TerminalHop("pty_read", "PRIVATE_RESOURCE", 12, 23, time.UnixMicro(123456))
	stop := StartTerminalRuntimeCapture("test_capture")
	stop()
	if err := diagnosticlog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{`"range_start":12`, `"range_end":23`, `"observed_unix_us":123456`, `"gc_count":`, `"reason":"test_capture"`, `"hop_source_file":"terminal_hops_test.go"`} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("missing capture contract %s", wanted)
		}
	}
	if strings.Contains(output.String(), "PRIVATE_RESOURCE") {
		t.Fatal("resource identity leaked")
	}
}

func TestRuntimePauseDeltaExcludesOldPausesAndBoundsNewOnes(t *testing.T) {
	h := &metrics.Float64Histogram{Buckets: []float64{0, .001, .01, math.Inf(1)}, Counts: []uint64{4, 2, 0}}
	if got := histogramDeltaUpperUS(h, []uint64{4, 1, 0}); got != 10000 {
		t.Fatalf("pause bound %d", got)
	}
	if got := histogramDeltaUpperUS(h, []uint64{4, 2, 0}); got != 0 {
		t.Fatalf("old pauses counted %d", got)
	}
	h.Counts[2]++
	if got := histogramDeltaUpperUS(h, []uint64{4, 2, 0}); got != math.MaxUint64 {
		t.Fatalf("unbounded pause understated %d", got)
	}
}

func TestConnectionHopLabelCorrelatesOppositeDirections(t *testing.T) {
	forward := ConnectionHopLabel("[fd00::1]:1234", "[fd00::2]:4321")
	reverse := ConnectionHopLabel("[fd00::2]:4321", "[fd00::1]:1234")
	if forward != reverse || len(forward) != 16 || strings.Contains(forward, "fd00") {
		t.Fatal("connection label lost correlation or privacy")
	}
}
