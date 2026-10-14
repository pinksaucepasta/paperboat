// Package diagnosticlog provides best-effort structured diagnostics that never
// block the operation being observed.
package diagnosticlog

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const queueDepth = 256

type record struct {
	level   slog.Level
	message string
	args    []any
	barrier chan struct{}
	pc      uintptr
	at      time.Time
}

var (
	start   sync.Once
	queue   = make(chan record, queueDepth)
	dropped atomic.Uint64
)

func ensureWorker() {
	start.Do(func() {
		go func() {
			for item := range queue {
				if item.barrier != nil {
					close(item.barrier)
					continue
				}
				handler := slog.Default().Handler()
				ctx := context.Background()
				if handler.Enabled(ctx, item.level) {
					record := slog.NewRecord(item.at, item.level, item.message, item.pc)
					record.Add(item.args...)
					if err := handler.Handle(ctx, record); err != nil {
						dropped.Add(1)
					}
				}
			}
		}()
	})
}

// TryInfo enqueues one record without waiting for the logging backend.
func TryInfo(message string, args ...any) {
	tryLog(slog.LevelInfo, message, args...)
}

func TryWarn(message string, args ...any)  { tryLog(slog.LevelWarn, message, args...) }
func TryError(message string, args ...any) { tryLog(slog.LevelError, message, args...) }

func tryLog(level slog.Level, message string, args ...any) {
	ensureWorker()
	if len(message) > 512 {
		message = message[:512]
	}
	item := record{level: level, message: strings.Clone(message), args: boundedArgs(args)}
	var callers [1]uintptr
	if runtime.Callers(3, callers[:]) != 0 {
		item.pc = callers[0]
	}
	item.at = time.Now()
	select {
	case queue <- item:
	default:
		dropped.Add(1)
	}
}

// Keep the queue bounded in bytes as well as record count. Payload objects,
// arbitrary errors and LogValuers must be projected by their owning caller.
func boundedArgs(args []any) []any {
	if len(args) > 32 {
		args = args[:32]
	}
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "", 0)
	r.Add(args...)
	result := make([]any, 0, 16)
	r.Attrs(func(a slog.Attr) bool {
		if len(a.Key) > 64 {
			return true
		}
		switch a.Value.Kind() {
		case slog.KindString:
			value := a.Value.String()
			if len(value) > 128 {
				value = value[:128]
			}
			a.Value = slog.StringValue(strings.Clone(value))
		case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool, slog.KindDuration, slog.KindTime:
		default:
			return true
		}
		a.Key = strings.Clone(a.Key)
		result = append(result, a)
		return len(result) < 16
	})
	return result
}

// Dropped reports records rejected by the queue or lost to a handler failure.
func Dropped() uint64 { return dropped.Load() }

// Flush waits for records already queued, bounded by the caller's deadline.
// A timed-out flush does not stop the process-owned worker or discard its queue.
func Flush(ctx context.Context) error {
	ensureWorker()
	barrier := make(chan struct{})
	select {
	case queue <- record{barrier: barrier}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
