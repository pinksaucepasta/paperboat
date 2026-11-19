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
// not delayed by the workspace's terminal cap (within terminalInteractiveGrace). Bulk output and uploads are larger.
const terminalInteractiveBytes = 1024

// terminalInteractiveGrace is how much debt (in time at the bucket's rate) an interactive frame passes through at once: what the bulk
// released ahead of an echo left behind. Past it the frame waits like bulk, so a client that sends bulk (or file bytes it did not mark)
// in small frames stays held to the workspace's bucket; typing never meets it.
const terminalInteractiveGrace = time.Second

// terminalPacing paces the frames of one browser terminal with its workspace's terminal cap (Throttles, the Terminal entries; the
// terminal's runtime host names the workspace): all the workspace's terminals share ONE bucket, so more tabs do not multiply the cap.
// It is an anti-abuse cap only (a terminal at a few kbit/s never notices it; it stops bulk data going through a terminal at wire
// speed); the allowance neither counts nor slows terminals. A capped terminal must stay responsive: the inner TLS stream cannot
// be reordered, so a small frame behind a bulk frame that is waiting for tokens would wait with it. The pacer therefore reads ahead
// (up to terminalReadAheadBytes) while a bulk frame waits, and an interactive frame it finds releases all the bulk ahead of it at once;
// their tokens stay taken, so the workspace is in debt and the next bulk frame waits for that debt. The average rate holds, and input
// echo is delayed by reading and writing, not by the throttle. Limit: an echo behind more than terminalReadAheadBytes of bulk output
// waits for that backlog at the throttled rate (the stream is ordered and TLS-encrypted end to end, so the edge cannot reorder it).
type terminalPacing struct {
	throttles *Throttles
	host      string
	// fileHost is the key of the workspace's ALLOWANCE bucket for this terminal's file transfers (listed by the server only while the
	// workspace is past its included traffic); fileBytes, when set, counts every file-marked frame toward the allowance.
	fileHost  string
	fileBytes func(n int)
}

// terminalFrame is one frame of a direction; file is set on the browser's file-transfer records (browserTerminalFileDiscriminator).
type terminalFrame struct {
	payload []byte
	file    bool
}

// interactive reports a frame that is never delayed by a bucket's rate: small, and not a file record (small file records are counted
// and slowed like bulk, or a stream of tiny files would count without ever being slowed).
func (f terminalFrame) interactive() bool {
	return len(f.payload) <= terminalInteractiveBytes && !f.file
}

// charge takes the frame's bytes without waiting: from the terminal cap, and for a file frame also from the allowance bucket (counted).
func (p terminalPacing) charge(f terminalFrame) {
	p.throttles.Charge(p.host, len(f.payload))
	if f.file {
		p.throttles.Charge(p.fileHost, len(f.payload))
		if p.fileBytes != nil {
			p.fileBytes(len(f.payload))
		}
	}
}

// reserve takes the frame's bytes and returns how long to wait: the longer of the cap's and, for a file frame, the allowance's.
func (p terminalPacing) reserve(f terminalFrame) time.Duration {
	delay := p.throttles.Reserve(p.host, len(f.payload))
	if f.file {
		delay = max(delay, p.throttles.Reserve(p.fileHost, len(f.payload)))
		if p.fileBytes != nil {
			p.fileBytes(len(f.payload))
		}
	}
	return delay
}

// start runs one direction: frames sent on the returned channel are forwarded in order, paced. Close the channel when the source ends;
// done is closed after the last frame was forwarded (or the context ended). A forward error ends the direction after report.
func (p terminalPacing) start(ctx context.Context, workers *sync.WaitGroup, report func(error), forward func([]byte) error) (chan<- terminalFrame, <-chan struct{}) {
	in := make(chan terminalFrame)
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

func (p terminalPacing) run(ctx context.Context, in <-chan terminalFrame, forward func([]byte) error) error {
	var pending []terminalFrame // frames read while a bulk frame waited, in order
	// release counts the next frames (the current one and those pending) that go at once: the bulk ahead of an interactive frame.
	release := 0
	closed := false
	for {
		var frame terminalFrame
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
		if release > 0 {
			p.charge(frame)
			release--
		} else if frame.interactive() {
			// Passes at once unless the bucket is deep in debt (a client sending small frames at wire speed).
			if delay := p.reserve(frame) - terminalInteractiveGrace; delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		} else if delay := p.reserve(frame); delay > 0 {
			// Wait for the tokens, but not in front of an interactive frame: read ahead, and stop waiting when one arrives.
			timer := time.NewTimer(delay)
			source := (<-chan terminalFrame)(in)
			ahead := len(frame.payload)
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
					ahead += len(next.payload)
					if next.interactive() {
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
		if err := forward(frame.payload); err != nil {
			return err
		}
	}
}
