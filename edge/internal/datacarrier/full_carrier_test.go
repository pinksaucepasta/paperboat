package datacarrier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// F21-168: a carrier takes its configured default stream count (16,384), all open at once (an account with 1,000
// previews and tunnels reaches the edge over its carrier pair): every stream does a round trip while all are
// held open, the next open is refused with the bounded stream-limit error, and closing the streams returns every
// permit.
func TestOneCarrierHoldsTheDefaultStreamsAtOnce(t *testing.T) {
	// The production time limits: the short ones of testCarrierConfig (1 s to write, a keepalive every 20 ms)
	// close a stream or the whole carrier when a loaded runner schedules 16,384 streams late.
	config := testCarrierConfig(defaultMaximumStreams)
	config.KeepAliveInterval, config.ConnectionWriteLimit = defaultKeepAliveInterval, defaultWriteLimit
	config.StreamOpenLimit, config.StreamCloseLimit = defaultOpenLimit, defaultCloseLimit
	edge, connector := dataCarrierPair(t, config)
	go func() {
		for {
			stream, _, err := connector.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				defer stream.Close()
				buffer := make([]byte, 4)
				if _, err := io.ReadFull(stream, buffer); err == nil {
					_ = writeAll(stream, []byte("pong"))
					_, _ = io.Copy(io.Discard, stream) // held open until the edge closes it
				}
			}()
		}
	}()
	// An opened stream lives only as long as this context, so it covers every open, every round trip and the
	// time all streams are held. Its bound only has to outlast a loaded -race runner (over 20 s there).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	release := make(chan struct{})
	var open, done sync.WaitGroup
	errs := make(chan error, defaultMaximumStreams)
	for i := range defaultMaximumStreams {
		open.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			stream, err := edge.OpenStream(ctx, testStreamOpen("route-a", fmt.Sprintf("request-%d", i)))
			// Each failure is sent before open.Done: errs is closed as soon as open.Wait returns.
			if err != nil {
				errs <- fmt.Errorf("stream %d: %w", i, err)
				open.Done()
				return
			}
			defer stream.Close()
			reply := make([]byte, 4)
			if err := writeAll(stream, []byte("ping")); err != nil {
				errs <- fmt.Errorf("stream %d write: %w", i, err)
				open.Done()
				return
			}
			if _, err := io.ReadFull(stream, reply); err != nil || string(reply) != "pong" {
				errs <- fmt.Errorf("stream %d round trip: %q %v", i, reply, err)
				open.Done()
				return
			}
			open.Done()
			<-release
		}()
	}
	open.Wait()
	close(errs)
	for err := range errs {
		close(release)
		done.Wait()
		t.Fatal(err)
	}
	if active := edge.ActiveStreams(); active != defaultMaximumStreams {
		t.Errorf("%d streams are open, want %d", active, defaultMaximumStreams)
	}
	if _, err := edge.OpenStream(context.Background(), testStreamOpen("route-a", "request-over-the-limit")); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("stream %d: %v, want the stream limit", defaultMaximumStreams+1, err)
	}
	close(release)
	done.Wait()
	if !waitForCarrier(func() bool { return edge.ActiveStreams() == 0 && connector.ActiveStreams() == 0 }) {
		t.Fatalf("stream permits leaked: edge=%d connector=%d", edge.ActiveStreams(), connector.ActiveStreams())
	}
}
