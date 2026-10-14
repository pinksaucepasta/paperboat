package diagnosticlog

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingHandler struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	levels  []slog.Level
	sources []string
}

func (h *blockingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockingHandler) Handle(_ context.Context, r slog.Record) error {
	h.once.Do(func() { close(h.entered); <-h.release })
	h.mu.Lock()
	h.levels = append(h.levels, r.Level)
	frames := runtime.CallersFrames([]uintptr{r.PC})
	frame, _ := frames.Next()
	h.sources = append(h.sources, frame.Function)
	h.mu.Unlock()
	return nil
}
func (h *blockingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockingHandler) WithGroup(string) slog.Handler      { return h }

func TestQueueDropsAreVisibleAndFlushHasDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}
	previous := slog.Default()
	h := &blockingHandler{entered: make(chan struct{}), release: make(chan struct{})}
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(previous)
	var release sync.Once
	defer release.Do(func() { close(h.release) })
	TryWarn("bounded diagnostic")
	select {
	case <-h.entered:
	case <-ctx.Done():
		t.Fatal("backend never entered")
	}
	before := Dropped()
	for range queueDepth + 2 {
		TryError("bounded failure")
	}
	if Dropped()-before != 2 {
		t.Fatalf("drops=%d", Dropped()-before)
	}
	deadline, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := Flush(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush=%v", err)
	}
	stop()
	release.Do(func() { close(h.release) })
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.levels) != queueDepth+1 || h.levels[0] != slog.LevelWarn || h.levels[1] != slog.LevelError {
		t.Fatal("severity or accepted records lost")
	}
	if !strings.HasSuffix(h.sources[0], ".TestQueueDropsAreVisibleAndFlushHasDeadline") {
		t.Fatalf("worker source replaced producer: %s", h.sources[0])
	}
}

type unsafeLogValue struct{}

func (unsafeLogValue) LogValue() slog.Value { panic("arbitrary LogValuer must not be resolved") }

func TestQueueRetainsOnlyBoundedPrimitiveAttributes(t *testing.T) {
	args := []any{"error", errors.New("PRIVATE"), "payload", map[string]string{"PRIVATE": "PRIVATE"}, "valuer", unsafeLogValue{}, "machine_id", "machine_01234567-89ab-4def-8123-456789abcdef", "count", uint64(3), "oversized", strings.Repeat("x", 4096)}
	safe := boundedArgs(args)
	if len(safe) != 3 {
		t.Fatalf("primitive attrs=%d", len(safe))
	}
	for _, raw := range safe {
		a := raw.(slog.Attr)
		if a.Value.Kind() == slog.KindString && len(a.Value.String()) > 128 {
			t.Fatal("string retained beyond byte bound")
		}
	}
}

func TestTryInfoNeverWaitsForBackend(t *testing.T) {
	started := time.Now()
	for range queueDepth * 4 {
		TryInfo("diagnostic test")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("nonblocking logging took %s", elapsed)
	}
}
