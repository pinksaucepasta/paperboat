package server

import (
	"errors"
	"testing"
)

func TestFrameBacklogIsBusyFromQueueingUntilServed(t *testing.T) {
	backlog := frameBacklog{queue: make(chan readResult, 2)}
	for range 2 {
		if err := backlog.enqueue(readResult{}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !backlog.busy() {
		t.Fatal("frames queued before the worker started read as idle")
	}
	close(backlog.queue)
	backlog.serve(func(readResult) error {
		if !backlog.busy() {
			t.Fatal("a frame being served reads as idle")
		}
		return nil
	}, nil, nil)
	if backlog.busy() {
		t.Fatal("a served backlog is still busy")
	}
}

func TestFrameBacklogReleasesRejectedAndAbandonedFrames(t *testing.T) {
	backlog := frameBacklog{queue: make(chan readResult, 2)}
	failed := make(chan error, 1)
	stopped := make(chan struct{})
	for range 2 {
		if err := backlog.enqueue(readResult{}, failed, stopped); err != nil {
			t.Fatal(err)
		}
	}
	close(stopped)
	if err := backlog.enqueue(readResult{}, failed, stopped); !errors.Is(err, ErrServerStopped) {
		t.Fatalf("enqueue on a full stopped queue: %v", err)
	}

	want := errors.New("frame write failed")
	close(backlog.queue)
	calls := 0
	backlog.serve(func(readResult) error {
		calls++
		return want
	}, failed, func() {})
	if calls != 1 || <-failed != want || backlog.busy() {
		t.Fatalf("failed worker handled %d frames, busy=%v", calls, backlog.busy())
	}
}
