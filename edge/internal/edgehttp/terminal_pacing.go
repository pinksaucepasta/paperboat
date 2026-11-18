package edgehttp

import (
	"context"
	"sync"
	"time"
)

// terminalReadAheadBytes bounds what a waiting pacer reads ahead to find an interactive frame behind the bulk it holds; past it the
// source waits (TCP backpressure to the host) and the throttle holds the order as it is.
const terminalReadAheadBytes = 256 << 10

// terminalInteractiveBytes is the size up to which a terminal frame is interactive (a keystroke, its echo, a resize, a prompt): it is
// never delayed by the workspace's throttle. Bulk output and uploads are larger.
const terminalInteractiveBytes = 1024

// terminalPacing paces the frames of one browser terminal with its workspace's throttle (Throttles; the terminal's runtime host
// names the workspace). The throttle applies to terminals like any public traffic (a terminal at a few kbit/s never notices it, and
// it stops bulk data going through a terminal for free), but a throttled terminal must stay responsive: the inner TLS stream cannot
// be reordered, so a small frame behind a bulk frame that is waiting for tokens would wait with it. The pacer therefore reads ahead
// (up to terminalReadAheadBytes) while a bulk frame waits, and an interactive frame it finds releases all the bulk ahead of it at once;
// their tokens stay taken, so the workspace is in debt and the next bulk frame waits for that debt. The average rate holds, and input
// echo is delayed by reading and writing, not by the throttle. Limit: an echo behind more than terminalReadAheadBytes of bulk output
// waits for that backlog at the throttled rate (the stream is ordered and TLS-encrypted end to end, so the edge cannot reorder it).
type terminalPacing struct {
	throttles *Throttles
	host      string
}

// start runs one direction: frames sent on the returned channel are forwarded in order, paced. Close the channel when the source ends;
// done is closed after the last frame was forwarded (or the context ended). A forward error ends the direction after report.
func (p terminalPacing) start(ctx context.Context, workers *sync.WaitGroup, report func(error), forward func([]byte) error) (chan<- []byte, <-chan struct{}) {
	in := make(chan []byte)
	done := make(chan struct{})
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(done)
		if err := p.run(ctx, in, forward); err != nil {
			report(err)
		}
	}()
	return in, done
}

func (p terminalPacing) run(ctx context.Context, in <-chan []byte, forward func([]byte) error) error {
	var pending [][]byte // frames read while a bulk frame waited, in order
	// release counts the next frames (the current one and those pending) that go at once: the bulk ahead of an interactive frame.
	release := 0
	closed := false
	for {
		var frame []byte
		switch {
		case len(pending) > 0:
			frame, pending = pending[0], pending[1:]
		case closed:
			return nil
		default:
			select {
			case next, ok := <-in:
				if !ok {
					return nil
				}
				frame = next
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if len(frame) <= terminalInteractiveBytes || release > 0 {
			p.throttles.Charge(p.host, len(frame))
			release = max(release-1, 0)
		} else if delay := p.throttles.Reserve(p.host, len(frame)); delay > 0 {
			// Wait for the tokens, but not in front of an interactive frame: read ahead, and stop waiting when one arrives.
			timer := time.NewTimer(delay)
			source := (<-chan []byte)(in)
			ahead := len(frame)
			for waiting := true; waiting; {
				select {
				case <-timer.C:
					waiting = false
				case next, ok := <-source:
					if !ok {
						source, closed = nil, true
						continue
					}
					pending = append(pending, next)
					ahead += len(next)
					if len(next) <= terminalInteractiveBytes {
						release, waiting = len(pending), false // the bulk waiting now goes below; with the frames up to the interactive one
					} else if ahead >= terminalReadAheadBytes {
						source = nil
					}
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			}
			timer.Stop()
		}
		if err := forward(frame); err != nil {
			return err
		}
	}
}
