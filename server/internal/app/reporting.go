package app

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
)

// Worker lifecycle metadata never contains the returned error or worker input.
// A worker panic becomes a supervised failure so dependencies still drain.
func observedWorker(reporter *supportreport.Service, name string, worker workers.Worker, progress ...*workers.Progress) workers.Worker {
	var tracker *workers.Progress
	if len(progress) > 0 {
		tracker = progress[0]
	}
	tracker.Register(name)
	return func(ctx context.Context) (err error) {
		if reporter == nil {
			ctx = tracker.Context(ctx, name)
			defer func() { tracker.Exit(name, workerExitWasCanceled(ctx, name, err)) }()
			return worker(ctx)
		}
		reference := supportreport.NewReference()
		ctx = reporter.Context(ctx, reference)
		ctx = tracker.Context(ctx, name, workerIterationObserver(reporter, name))
		defer func() { tracker.Exit(name, workerExitWasCanceled(ctx, name, err)) }()
		reporter.Operation(ctx, name, "state_change", reference, -1)
		started := time.Now()
		defer func() {
			outcome := "success"
			if value := recover(); value != nil {
				err = errors.New("background worker panicked")
				fault := reporter.CaptureFailureWithStack(ctx, "paperboat-server", name, "worker_exit", "worker_panic", supportreport.ErrPanic, sentry.NewStacktrace())
				reporter.LogFault(ctx, slog.LevelError, "background worker panicked", fault)
				outcome = "failed"
			} else if workerExitWasCanceled(ctx, name, err) {
				outcome = "canceled"
			} else {
				failure := err
				if failure == nil && ctx.Err() != nil {
					failure = ctx.Err()
				}
				if failure != nil {
					outcome = "failed"
					fault := reporter.CaptureFailure(ctx, "paperboat-server", name, "worker_exit", "worker_failed", failure)
					reporter.LogFault(ctx, slog.LevelError, "background worker failed", fault)
				}
			}
			reporter.Operation(ctx, name, outcome, reference, time.Since(started))
		}()
		return worker(ctx)
	}
}

func workerExitWasCanceled(ctx context.Context, name string, err error) bool {
	if ctx == nil || ctx.Err() != context.Canceled {
		return false
	}
	return supportreport.ProjectFault(ctx, "paperboat-server", name, "worker_exit", "worker_failed", err).Cause == "context_canceled"
}

func workerIterationObserver(reporter *supportreport.Service, name string) workers.IterationObserver {
	var mu sync.Mutex
	lastClass := ""
	activeFailure := false
	return func(ctx context.Context, iteration workers.IterationFault, recovered bool) {
		if recovered {
			mu.Lock()
			if !activeFailure {
				mu.Unlock()
				return
			}
			activeFailure, lastClass = false, ""
			mu.Unlock()
			fault := reporter.CaptureFailure(ctx, "paperboat-server", name, iteration.Stage, iteration.Code, nil)
			reporter.LogFault(ctx, slog.LevelInfo, "background worker recovered", fault)
			return
		}
		if iteration.Err == nil {
			return
		}
		projected := supportreport.ProjectFault(ctx, "paperboat-server", name, iteration.Stage, iteration.Code, iteration.Err)
		class := strings.Join([]string{projected.Stage, projected.Code, projected.Cause, projected.ErrorType, strings.Join(projected.ErrorChain, ","), strconv.Itoa(projected.Errno)}, "\x00")
		mu.Lock()
		if activeFailure && class == lastClass {
			mu.Unlock()
			return
		}
		activeFailure, lastClass = true, class
		mu.Unlock()
		fault := reporter.CaptureFailure(ctx, "paperboat-server", name, iteration.Stage, iteration.Code, iteration.Err)
		level := slog.LevelError
		if fault.Severity == "warning" {
			level = slog.LevelWarn
		} else if fault.Severity == "info" {
			level = slog.LevelInfo
		}
		reporter.LogFault(ctx, level, "background worker iteration failed", fault)
	}
}
