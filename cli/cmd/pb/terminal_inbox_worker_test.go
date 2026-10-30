package main

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

func TestTerminalInboxReplacementJoinsPreviousPollerAndKeepsFailure(t *testing.T) {
	ctx := supportref.WithContext(t.Context(), supportref.New())
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	t.Cleanup(restore)
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var worker terminalInboxWorker
	failed := func(err error) { reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", err) }
	worker.start(ctx, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return errors.Join(ctx.Err(), syscall.EIO)
	}, failed)
	<-started
	freshStarted, replacementDone := make(chan struct{}), make(chan struct{})
	go func() {
		worker.start(ctx, func(ctx context.Context) error { close(freshStarted); <-ctx.Done(); return ctx.Err() }, failed)
		close(replacementDone)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		close(release)
		worker.stop()
		t.Fatal("replacement did not cancel old poller")
	}
	select {
	case <-freshStarted:
		close(release)
		worker.stop()
		t.Fatal("new poller started before old worker joined")
	default:
	}
	close(release)
	select {
	case <-replacementDone:
	case <-time.After(time.Second):
		worker.stop()
		t.Fatal("replacement did not finish")
	}
	worker.stop()
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != supportref.FromContext(ctx) {
		t.Fatal("inbox shutdown lost mixed I/O evidence or recorded pure cancellation")
	}
}

func TestTerminalAuxiliaryReplacementDoesNotDelayReadyTerminal(t *testing.T) {
	var worker terminalInboxWorker
	started, release := make(chan struct{}), make(chan struct{})
	worker.start(t.Context(), func(ctx context.Context) error { close(started); <-ctx.Done(); <-release; return ctx.Err() }, func(error) {})
	<-started
	replaced, freshStarted, freshDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		worker.start(t.Context(), func(ctx context.Context) error {
			defer close(freshDone)
			close(freshStarted)
			<-ctx.Done()
			return ctx.Err()
		}, func(error) {})
		close(replaced)
	}()
	select {
	case <-replaced:
	case <-time.After(time.Second):
		close(release)
		worker.stop()
		t.Fatal("ready terminal waited for the previous auxiliary request to finish")
	}
	select {
	case <-freshStarted:
		t.Fatal("replacement setup overlapped the previous resource owner")
	default:
	}
	close(release)
	select {
	case <-freshStarted:
	case <-time.After(time.Second):
		worker.stop()
		t.Fatal("replacement setup did not start after previous cleanup")
	}
	worker.stop()
	select {
	case <-freshDone:
	default:
		t.Fatal("terminal exit did not join auxiliary setup")
	}
}
