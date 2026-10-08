package native

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPeerOperationFailurePreservesCauseAndUsesStaticMetadata(t *testing.T) {
	cause := errors.New("private peer address and credential")
	err := classifyPeerOperationFailure(context.Background(), "peer_connect", "native_private_failed", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("wrapped failure lost cause: %v", err)
	}
	var staged interface{ DiagnosticStage() string }
	var coded interface{ DiagnosticCode() string }
	if !errors.As(err, &staged) || staged.DiagnosticStage() != "peer_connect" ||
		!errors.As(err, &coded) || coded.DiagnosticCode() != "native_private_failed" {
		t.Fatalf("diagnostic metadata missing: %T %v", err, err)
	}
	if strings.Contains(err.Error(), "private peer address") || err.Error() != "native private peer connection failed" {
		t.Fatalf("failure text was not static: %q", err)
	}
}

func TestPeerListenerAcceptFailureKeepsItsActualPhase(t *testing.T) {
	cause := errors.New("private peer address")
	err := classifyPeerOperationFailure(context.Background(), "listener_accept", "native_private_failed", cause)
	if !errors.Is(err, cause) || err.Error() != "native peer listener could not accept a connection" {
		t.Fatalf("listener failure text or cause incorrect: %v", err)
	}
	var staged interface{ DiagnosticStage() string }
	var coded interface{ DiagnosticCode() string }
	if !errors.As(err, &staged) || staged.DiagnosticStage() != "listener_accept" || !errors.As(err, &coded) || coded.DiagnosticCode() != "native_private_failed" {
		t.Fatalf("listener accept phase missing: %T %v", err, err)
	}
}

func TestPeerOperationFailureLeavesCancellationUnwrapped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := classifyPeerOperationFailure(ctx, "peer_connect", "native_private_failed", errors.New("transport detail"))
	if err != context.Canceled {
		t.Fatalf("canceled operation error=%v, want original context cancellation", err)
	}
}

func TestPeerOperationFailurePreservesCustomCancellationCauseAndStatus(t *testing.T) {
	cause := errors.New("caller stopped peer connection")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	err := classifyPeerOperationFailure(ctx, "peer_connect", "native_private_failed", errors.New("transport detail"))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("cancellation status or cause lost: %v", err)
	}
	var staged interface{ DiagnosticStage() string }
	if errors.As(err, &staged) {
		t.Fatalf("canceled operation gained a diagnostic phase: %T %v", err, err)
	}
}

func TestPeerOperationFailureKeepsDeadlineAndPhase(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cause := errors.New("private transport detail")
	err := classifyPeerOperationFailure(ctx, "peer_connect", "native_private_failed", cause)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
		t.Fatalf("deadline or transport cause lost: %v", err)
	}
	var staged interface{ DiagnosticStage() string }
	if !errors.As(err, &staged) || staged.DiagnosticStage() != "peer_connect" {
		t.Fatalf("deadline phase missing: %T %v", err, err)
	}
}

func TestDialFailureEventCarriesReturnedClassificationAndSkipsShutdown(t *testing.T) {
	cause := errors.New("private peer endpoint")
	var events []Event
	owner := &Owner{observe: func(event Event) { events = append(events, event) }}
	failure := owner.dialFailure(context.Background(), "machine_private", "peer_authority", "peer_authority_failed", cause)
	if len(events) != 1 || events[0].Kind != "dial_failed" || events[0].Err != failure || !errors.Is(failure, cause) {
		t.Fatalf("dial event/error mismatch: event=%+v failure=%v", events, failure)
	}
	var staged interface{ DiagnosticStage() string }
	var coded interface{ DiagnosticCode() string }
	if !errors.As(events[0].Err, &staged) || staged.DiagnosticStage() != "peer_authority" ||
		!errors.As(events[0].Err, &coded) || coded.DiagnosticCode() != "peer_authority_failed" {
		t.Fatalf("dial event lost typed phase: %T %v", events[0].Err, events[0].Err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failure = owner.dialFailure(ctx, "machine_private", "peer_connect", "native_private_failed", cause)
	if len(events) != 1 || !errors.Is(failure, context.Canceled) {
		t.Fatalf("shutdown produced an error event or lost cancellation: events=%+v failure=%v", events, failure)
	}
}

func TestNormalPeerServeTerminationDoesNotBecomeUnexpectedFailure(t *testing.T) {
	for _, err := range []error{nil, io.EOF, net.ErrClosed, context.Canceled} {
		if !normalPeerServeTermination(err) {
			t.Fatalf("normal peer termination %v was reported as a worker failure", err)
		}
	}
	if normalPeerServeTermination(errors.New("protocol failure")) {
		t.Fatal("unexpected application failure was hidden")
	}
}

func TestNormalPeerServeTerminationRequiresBoundedAllNormalCauses(t *testing.T) {
	if !normalPeerServeTermination(errors.Join(io.EOF, net.ErrClosed, context.Canceled)) {
		t.Fatal("joined normal stream termination was reported as a worker failure")
	}
	if normalPeerServeTermination(errors.Join(context.Canceled, errors.New("handler failed"))) {
		t.Fatal("substantive failure joined with cancellation was hidden")
	}

	cycle := &peerCycleError{}
	cycle.cause = cycle
	if normalPeerServeTermination(cycle) {
		t.Fatal("cyclic custom error chain was treated as normal termination")
	}
	var typedNil error = (*peerCycleError)(nil)
	if normalPeerServeTermination(typedNil) {
		t.Fatal("typed nil error was treated as normal termination")
	}
	deep := error(io.EOF)
	for range 17 {
		deep = &peerCycleError{cause: deep}
	}
	if normalPeerServeTermination(deep) {
		t.Fatal("error chain beyond the traversal bound was treated as normal termination")
	}

	multi := peerCyclicErrors{nil}
	multi[0] = multi
	if normalPeerServeTermination(multi) {
		t.Fatal("cyclic non-comparable multi-error was treated as normal termination")
	}
}

type peerCycleError struct{ cause error }

func (*peerCycleError) Error() string   { return "peer operation failed" }
func (e *peerCycleError) Unwrap() error { return e.cause }

type peerCyclicErrors []error

func (peerCyclicErrors) Error() string     { return "peer operations failed" }
func (e peerCyclicErrors) Unwrap() []error { return []error(e) }
