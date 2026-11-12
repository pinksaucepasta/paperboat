package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A replacement server reports ready only once it is warm, so the rollout's "ready" means "answers in normal time".
func TestServerIsNotReadyUntilWarm(t *testing.T) {
	warmed := new(atomic.Bool)
	checker := readinessChecker{draining: new(atomic.Bool), warmed: warmed}
	if err := checker.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "warming up") {
		t.Fatalf("a server that has not warmed up is ready: %v", err)
	}
	release, started := make(chan struct{}), make(chan struct{})
	app := &App{warmed: warmed, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), warm: func(context.Context) { close(started); <-release }}
	done := make(chan struct{})
	go func() { app.runWarmUp(context.Background()); close(done) }()
	<-started
	if warmed.Load() {
		t.Fatal("ready while the warm-up is still running")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the warm-up did not finish")
	}
	if !warmed.Load() {
		t.Fatal("not ready after the warm-up")
	}
}

func TestServerWithoutWarmUpIsReadyAfterRun(t *testing.T) {
	warmed := new(atomic.Bool)
	app := &App{warmed: warmed, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app.runWarmUp(context.Background())
	if !warmed.Load() {
		t.Fatal("a server with no warm-up never became ready")
	}
}
