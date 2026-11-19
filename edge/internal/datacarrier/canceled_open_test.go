package datacarrier

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	yamux "github.com/libp2p/go-yamux/v5"
)

// blockedOpenSession holds OpenStream until released, then fails the way the yamux adapter does: an
// error together with a nil *yamux.Stream wrapped in the StreamLink interface (not a nil interface).
type blockedOpenSession struct {
	release chan struct{}
	closed  chan struct{}
	entered chan struct{} // closed when OpenStream is first called
	once    sync.Once
}

func (s *blockedOpenSession) OpenStream(context.Context) (StreamLink, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	var none *yamux.Stream
	return none, errors.New("session is shutting down")
}
func (s *blockedOpenSession) AcceptStream(ctx context.Context) (StreamLink, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *blockedOpenSession) Ping(context.Context) error { return nil }
func (s *blockedOpenSession) Close() error               { return nil }
func (s *blockedOpenSession) CloseChan() <-chan struct{} { return s.closed }

// A canceled open whose underlying OpenStream later fails must release its permit and must not close
// the failed attempt's nil stream (that crashed the edge with a nil dereference in yamux).
func TestCanceledOpenThatLaterFailsReleasesItsPermitWithoutClosingANilStream(t *testing.T) {
	session := &blockedOpenSession{release: make(chan struct{}), closed: make(chan struct{}), entered: make(chan struct{})}
	server, err := NewServerWithSession(t.Context(), session, testCarrierConfig(4))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	opened := make(chan error, 1)
	go func() {
		_, err := server.OpenStream(ctx, testStreamOpen("route-a", "request-canceled"))
		opened <- err
	}()
	select { // cancel an open that is blocked in the session, not one that has not started
	case <-session.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the open never reached the session")
	}
	cancel()
	if err := <-opened; !errors.Is(err, context.Canceled) {
		t.Fatalf("open error = %v, want canceled", err)
	}
	close(session.release)
	if !waitForCarrier(func() bool { return server.ActiveStreams() == 0 }) {
		t.Fatalf("canceled open kept %d stream permits", server.ActiveStreams())
	}
}
