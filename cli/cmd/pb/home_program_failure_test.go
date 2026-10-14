package main

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestHomeProgramFailurePreservesCancellationOwnership(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	killedCanceled := fmt.Errorf("%w: %w", tea.ErrProgramKilled, context.Canceled)
	killedDeadline := fmt.Errorf("%w: %w", tea.ErrProgramKilled, context.DeadlineExceeded)
	if got := homeProgramFailure(canceled, killedCanceled); got != context.Canceled {
		t.Fatal("pure caller cancellation was not normalized")
	}
	if got := homeProgramFailure(deadline, killedDeadline); got != context.DeadlineExceeded {
		t.Fatal("deadline became cancellation")
	}
	if got := homeProgramFailure(context.Background(), tea.ErrProgramKilled); got != tea.ErrProgramKilled {
		t.Fatal("internal kill became caller cancellation")
	}
	if got := homeProgramFailure(canceled, errors.Join(killedCanceled, killedCanceled)); got != context.Canceled {
		t.Fatal("duplicate cancellation lost expected outcome")
	}
	mixed := errors.Join(killedCanceled, syscall.EIO)
	got := homeProgramFailure(canceled, mixed)
	if !errors.Is(got, mixed) || !errors.Is(got, syscall.EIO) || !errors.Is(got, context.Canceled) {
		t.Fatal("independent failure was discarded")
	}
	got = homeProgramFailure(canceled, killedDeadline)
	if !errors.Is(got, context.DeadlineExceeded) || !errors.Is(got, context.Canceled) {
		t.Fatal("independent timeout was discarded")
	}
	causeCtx, cancelCause := context.WithCancelCause(context.Background())
	cancelCause(syscall.EIO)
	got = homeProgramFailure(causeCtx, killedCanceled)
	if !errors.Is(got, killedCanceled) || !errors.Is(got, syscall.EIO) || !errors.Is(got, context.Canceled) {
		t.Fatal("context cause was discarded")
	}
	spy := &connectProofError{cause: syscall.EIO}
	got = homeProgramFailure(canceled, spy)
	if !errors.Is(got, spy) || !errors.Is(got, syscall.EIO) {
		t.Fatal("private original cause was discarded")
	}
	cycle := &connectProofError{}
	cycle.cause = cycle
	if got := homeProgramFailure(canceled, cycle); got == context.Canceled {
		t.Fatal("cycle became normal cancellation")
	}
	var typedNil *connectProofError
	if got := homeProgramFailure(canceled, typedNil); got == context.Canceled {
		t.Fatal("typed nil became normal cancellation")
	}
	if got := homeProgramFailure(canceled, nil); got != nil {
		t.Fatal("successful program became failure")
	}
}
