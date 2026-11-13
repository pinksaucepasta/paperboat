// Package serverclock keeps this process's view of the Paperboat server's clock.
//
// Server credentials carry absolute expiry times and machine proofs carry the local time; the server
// accepts proofs within ±60 s. A machine whose clock is a minute or more off would treat fresh grants
// as expired and have its proofs refused. Like Tailscale's clockDelta, the offset is learned from the
// Date of every Paperboat server response and applied to proofs and expiry checks once it reaches
// Threshold; below it the local clock is used as it is.
package serverclock

import (
	"errors"
	"net/http"
	"sync/atomic"
	"time"
)

// Threshold is the smallest offset applied. Date has one-second resolution, so smaller offsets are
// noise, and the server's proof window absorbs them.
const Threshold = time.Minute

// maxRoundTrip bounds the request used to measure: a slower one cannot place the server's time.
const maxRoundTrip = 10 * time.Second

var offset atomic.Int64

// reference is the local clock when the offset was learned: the monotonic reading says how long ago, the
// wall reading shows whether the local clock was stepped since (X-27: a clock set back makes the learned offset wrong).
type reference struct {
	mono time.Time
	wall int64
}

var (
	learned     atomic.Pointer[reference]
	remeasure   atomic.Pointer[func()]
	remeasured  atomic.Pointer[time.Time]
	wallNano    = func() int64 { return time.Now().UnixNano() } // replaced in tests
	remeasureIn = 10 * time.Second
)

// SetRemeasure registers how to ask the server for its time again (a request whose response goes to Observe). It
// runs, at most every 10 s, when the local clock was stepped since the offset was learned.
func SetRemeasure(f func()) { remeasure.Store(&f) }

// step is how far the local wall clock moved against the monotonic clock since the offset was learned.
func step() time.Duration {
	ref := learned.Load()
	if ref == nil {
		return 0
	}
	return time.Duration(wallNano()-ref.wall) - time.Since(ref.mono)
}

// Why a response could not place the server's clock.
var (
	ErrNoDate        = errors.New("the response has no Date header")
	ErrBadDate       = errors.New("the response Date header cannot be read")
	ErrSlowRoundTrip = errors.New("the request took too long to place the server's time")
)

// Measure is how far the server's clock is ahead of this machine's (negative: behind), or 0 below
// Threshold, from a response Date header for a request sent at sent and answered at received (both
// local clock). The errors say why no measurement was possible.
func Measure(date string, sent, received time.Time) (time.Duration, error) {
	if date == "" {
		return 0, ErrNoDate
	}
	server, err := http.ParseTime(date)
	if err != nil {
		return 0, ErrBadDate
	}
	elapsed := received.Sub(sent)
	if elapsed < 0 || elapsed > maxRoundTrip {
		return 0, ErrSlowRoundTrip
	}
	// Date truncates to the second: the server's time was within the half second after it.
	delta := server.Add(500 * time.Millisecond).Sub(sent.Add(elapsed / 2))
	if delta.Abs() < Threshold {
		delta = 0
	}
	return delta, nil
}

// Observe records the server's clock from a response Date header, for a request sent at sent and
// answered at received (both local clock).
func Observe(date string, sent, received time.Time) {
	if delta, err := Measure(date, sent, received); err == nil {
		offset.Store(int64(delta))
		learned.Store(&reference{mono: received, wall: wallNano()})
	}
}

// Learned is whether any server answer has placed the server's clock in this process yet.
func Learned() bool { return learned.Load() != nil }

// Offset is how far the server's clock is ahead of this machine's (negative: behind), or 0 below
// Threshold. When the local clock was stepped since it was learned (an NTP fix, a clock set back), or the
// machine slept, the offset may be stale: the server is asked for its time again, at most every 10 s, and
// the old offset stays until that answer arrives (a step and a sleep cannot be told apart).
func Offset() time.Duration {
	if s := step(); s.Abs() >= Threshold {
		if f, last := remeasure.Load(), remeasured.Load(); f != nil && (last == nil || time.Since(*last) >= remeasureIn) {
			now := time.Now()
			if remeasured.CompareAndSwap(last, &now) {
				go (*f)()
			}
		}
	}
	return time.Duration(offset.Load())
}

// Now is the current time on the server's clock.
func Now() time.Time { return time.Now().Add(Offset()) }

// Until is the server-clock duration until t.
func Until(t time.Time) time.Duration { return t.Sub(Now()) }
