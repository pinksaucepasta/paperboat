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

// F21-168: a carrier takes its configured stream count, 1,024 by default, all open at once (an account with 1,000
// previews and tunnels reaches the edge over its carrier pair): every stream does a round trip while all are
// held open, the next open is refused with the bounded stream-limit error, and closing the streams returns every
// permit.
func TestOneCarrierHoldsTheDefaultThousandAndTwentyFourStreamsAtOnce(t *testing.T) {
	edge, connector := dataCarrierPair(t, testCarrierConfig(defaultMaximumStreams))
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
	release := make(chan struct{})
	var open, done sync.WaitGroup
	errs := make(chan error, defaultMaximumStreams)
	for i := range defaultMaximumStreams {
		open.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			stream, err := edge.OpenStream(ctx, testStreamOpen("route-a", fmt.Sprintf("request-%d", i)))
			if err != nil {
				open.Done()
				errs <- fmt.Errorf("stream %d: %w", i, err)
				return
			}
			defer stream.Close()
			reply := make([]byte, 4)
			if err := writeAll(stream, []byte("ping")); err != nil {
				open.Done()
				errs <- fmt.Errorf("stream %d write: %w", i, err)
				return
			}
			if _, err := io.ReadFull(stream, reply); err != nil || string(reply) != "pong" {
				open.Done()
				errs <- fmt.Errorf("stream %d round trip: %q %v", i, reply, err)
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
