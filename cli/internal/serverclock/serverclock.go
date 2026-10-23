// Package serverclock keeps this process's view of the Paperboat server's clock.
//
// Server credentials carry absolute expiry times and machine proofs carry the local time; the server
// accepts proofs within ±60 s. A machine whose clock is a minute or more off would treat fresh grants
// as expired and have its proofs refused. Like Tailscale's clockDelta, the offset is learned from the
// Date of every Paperboat server response and applied to proofs and expiry checks once it reaches
// Threshold; below it the local clock is used as it is.
package serverclock

import (
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

// Observe records the server's clock from a response Date header, for a request sent at sent and
// answered at received (both local clock).
func Observe(date string, sent, received time.Time) {
	server, err := http.ParseTime(date)
	elapsed := received.Sub(sent)
	if date == "" || err != nil || elapsed < 0 || elapsed > maxRoundTrip {
		return
	}
	// Date truncates to the second: the server's time was within the half second after it.
	delta := server.Add(500 * time.Millisecond).Sub(sent.Add(elapsed / 2))
	if delta.Abs() < Threshold {
		delta = 0
	}
	offset.Store(int64(delta))
}

// Offset is how far the server's clock is ahead of this machine's (negative: behind), or 0 below
// Threshold.
func Offset() time.Duration { return time.Duration(offset.Load()) }

// Now is the current time on the server's clock.
func Now() time.Time { return time.Now().Add(Offset()) }

// Until is the server-clock duration until t.
func Until(t time.Time) time.Duration { return t.Sub(Now()) }
