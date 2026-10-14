package main

import (
	"context"
	"errors"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/statusbar"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type terminalPathWatchFixture struct {
	started, cleanup, release chan struct{}
}

func (f terminalPathWatchFixture) Snapshot(context.Context) (localapi.Snapshot, error) {
	return localapi.Snapshot{Generation: 1}, nil
}

func (f terminalPathWatchFixture) Watch(ctx context.Context, _ uint64) (<-chan localapi.Snapshot, <-chan error) {
	updates, failures := make(chan localapi.Snapshot), make(chan error, 1)
	go func() {
		close(f.started)
		<-ctx.Done()
		failures <- errors.Join(ctx.Err(), syscall.EIO)
		close(failures)
		close(f.cleanup)
		<-f.release
		close(updates)
	}()
	return updates, failures
}

func TestTerminalPathWatchJoinsCleanupAndKeepsMixedCancellationFailure(t *testing.T) {
	fixture := terminalPathWatchFixture{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithCancel(supportref.WithContext(t.Context(), supportref.New()))
	defer cancel()
	bar := statusbar.New(statusbar.Options{Mode: statusbar.ModeOff})
	defer bar.Close()
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	t.Cleanup(restore)
	done := make(chan struct{})
	go func() { defer close(done); watchMachineTransportPath(ctx, fixture, "machine", bar) }()
	<-fixture.started
	cancel()
	select {
	case <-fixture.cleanup:
	case <-time.After(time.Second):
		close(fixture.release)
		t.Fatal("watch did not cancel")
	}
	select {
	case <-done:
		close(fixture.release)
		t.Fatal("path watcher returned before stream cleanup joined")
	default:
	}
	close(fixture.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("path watcher did not join")
	}
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != supportref.FromContext(ctx) {
		t.Fatal("status watch lost final mixed I/O failure")
	}
}

func TestTerminalAuxHTTPWrappedCancellationIsQuiet(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	t.Cleanup(restore)
	err := &url.Error{Op: "PRIVATE", URL: "PRIVATE", Err: context.Canceled}
	if reportTerminalAuxFailure(t.Context(), "delivery", "file_transfer_failed", err) != "" || len(faults) != 0 {
		t.Fatal("orderly HTTP cancellation became file-transfer failure")
	}
}
