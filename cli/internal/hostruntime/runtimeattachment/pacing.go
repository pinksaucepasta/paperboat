package runtimeattachment

import "time"

// healthyRecheck bounds how long a healthy runtime carrier goes without asking the control plane
// for its attachment. The admission poll ticks every second; checking that often while nothing is
// wrong sent one POST per machine per second and kept the server's attachment rows hot.
const healthyRecheck = 30 * time.Second

// roundPacer decides which poll ticks run a full attachment round. While the carriers are healthy
// a round runs every healthyRecheck, or sooner when half of the admission's remaining life has
// passed; a carrier that stops being ready runs one at once. Failure backoff is the admission
// poll's own (consecutive failures wait longer, Retry-After is honoured), so rounds after a failure
// are not delayed further here.
type roundPacer struct {
	next    time.Time
	healthy bool
}

// due reports whether this tick runs a round; carriersReady is true when every carrier the last
// round connected is still ready and one of them publishes.
func (p *roundPacer) due(now time.Time, carriersReady bool) bool {
	if !p.healthy {
		return true
	}
	return !carriersReady || !now.Before(p.next)
}

// finished records a round. A healthy round (no error, every wanted carrier ready) waits until
// the next recheck; any other round keeps the next tick due.
func (p *roundPacer) finished(now time.Time, healthy bool, expiresAt time.Time) {
	p.healthy = healthy
	if !healthy {
		p.next = time.Time{}
		return
	}
	wait := healthyRecheck
	if remaining := expiresAt.Sub(now) / 2; remaining < wait {
		wait = remaining
	}
	p.next = now.Add(wait)
}
