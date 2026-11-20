package datacarrier

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// countedLink is a stream that counts its Close calls.
type countedLink struct {
	StreamLink
	closed *atomic.Int32
}

func (l *countedLink) Close() error { l.closed.Add(1); return nil }

// Both a won result and a closed carrier (or a cancelled context) are ready: select would pick one at random, so a stream the
// open or accept had won was closed or kept by a coin flip (the yamux wrappers and the Server and Client open and accept paths).
// The result must win every time.
func TestAwaitOpenPrefersAWonResultOverACloseOrACancel(t *testing.T) {
	done := make(chan struct{})
	close(done)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
		done <-chan struct{}
	}{{"closed carrier", t.Context(), done}, {"cancelled context", cancelled, make(chan struct{})}, {"both", cancelled, done}} {
		t.Run(test.name, func(t *testing.T) {
			want := &countedLink{closed: new(atomic.Int32)}
			for range 200 { // a select with two ready cases picks the wrong one about every other time
				result := make(chan openResult, 1)
				result <- openResult{stream: want}
				opened, err := awaitOpen(test.ctx, test.done, result)
				if err != nil || opened.stream != want {
					t.Fatalf("a won result was lost: stream=%v err=%v", opened.stream, err)
				}
			}
		})
	}
}

func TestAwaitOpenReportsTheCauseWhenNothingWasWon(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if _, err := awaitOpen(t.Context(), done, make(chan openResult, 1)); !errors.Is(err, ErrCarrierClosed) {
		t.Fatalf("closed carrier: %v, want ErrCarrierClosed", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := awaitOpen(cancelled, make(chan struct{}), make(chan openResult, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v, want context.Canceled", err)
	}
}

func TestCloseLateStreamClosesTheStreamAnAbandonedAcceptDelivers(t *testing.T) {
	closed := new(atomic.Int32)
	result := make(chan openResult, 1)
	finished := make(chan struct{})
	go func() { closeLateStream(result); close(finished) }()
	result <- openResult{stream: &countedLink{closed: closed}}
	<-finished
	if closed.Load() != 1 {
		t.Fatalf("the late stream was closed %d times, want 1", closed.Load())
	}
	failed := make(chan openResult, 1)
	failed <- openResult{err: errors.New("session is shutting down")} // a failed accept carries no stream to close
	closeLateStream(failed)
}

// A ping that succeeded stays a success when the context ends or the carrier closes at the same moment.
func TestPingOrKeepsAFinishedPing(t *testing.T) {
	for range 200 {
		result := make(chan error, 1)
		result <- nil
		if err := pingOr(result, context.Canceled); err != nil {
			t.Fatalf("a successful ping reported %v", err)
		}
	}
	failed := make(chan error, 1)
	failed <- errors.New("ping failed")
	if err := pingOr(failed, context.Canceled); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("a failed ping reported %v, want its own error", err)
	}
	if err := pingOr(make(chan error, 1), ErrCarrierClosed); !errors.Is(err, ErrCarrierClosed) {
		t.Fatalf("an unfinished ping reported %v, want ErrCarrierClosed", err)
	}
}

// immediateSession answers every open at once with a stream that counts its closes.
type immediateSession struct {
	blockedOpenSession
	opened, closed atomic.Int32
}

func (s *immediateSession) OpenStream(context.Context) (StreamLink, error) {
	s.opened.Add(1)
	return &countedLink{closed: &s.closed}, nil
}

// An open whose context is already cancelled ends the same way whichever of its ready cases select picks: context.Canceled,
// the stream it won closed, its permit released. (-count=50: the interleavings differ run to run.)
func TestOpenWithACancelledContextEndsTheSameWayWhateverSelectPicks(t *testing.T) {
	session := &immediateSession{blockedOpenSession: blockedOpenSession{closed: make(chan struct{})}}
	server, err := NewServerWithSession(t.Context(), session, testCarrierConfig(4))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	for range 20 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := server.OpenStream(ctx, testStreamOpen("route-a", "request-cancelled")); !errors.Is(err, context.Canceled) {
			t.Fatalf("open error = %v, want context.Canceled", err)
		}
	}
	if !waitForCarrier(func() bool { return server.ActiveStreams() == 0 && session.closed.Load() == session.opened.Load() }) {
		t.Fatalf("opened %d streams, closed %d, %d permits held", session.opened.Load(), session.closed.Load(), server.ActiveStreams())
	}
}
