// Package originhealth tracks whether the app a preview or tunnel route forwards to is
// reachable, from the connector's own dial results plus a small periodic probe, and reports
// only state changes. It exists so that an app that was stopped shows up as such in
// `pb preview list` and `pb tunnel status` instead of visitors silently getting a bare 502.
package originhealth

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"time"
)

// State is the last known reachability of one origin.
type State uint8

const (
	// Unknown means nothing has been observed yet.
	Unknown State = iota
	Up
	Down
)

// Reason is a finite, value-free description of why an origin is down.
type Reason string

const (
	ReasonRefused     Reason = "refused"
	ReasonTimeout     Reason = "timeout"
	ReasonUnreachable Reason = "unreachable"
	ReasonOther       Reason = "other"
)

const (
	// FailuresToDown is how many consecutive failed dials or probes make an origin Down. With the
	// default probe interval a stopped app is reported about 30 seconds later.
	FailuresToDown = 3
	// DefaultProbeInterval is how often an origin is probed even without visitor traffic.
	DefaultProbeInterval = 10 * time.Second
	// DefaultRepeatInterval is how often a Down state is re-reported so the server can age it out
	// when the host stops reporting.
	DefaultRepeatInterval = 30 * time.Second
)

// Event is one reported change of an origin's state (or a repeat of Down).
type Event struct {
	Key    string
	State  State
	Reason Reason
	At     time.Time
	Repeat bool
}

// Classify maps a dial error to its finite reason.
func Classify(err error) Reason {
	var timeout interface{ Timeout() bool }
	switch {
	case err == nil:
		return ""
	case errors.Is(err, syscall.ECONNREFUSED):
		return ReasonRefused
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return ReasonTimeout
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return ReasonUnreachable
	default:
		return ReasonOther
	}
}

type origin struct {
	state      State
	failures   int
	reason     Reason
	lastReport time.Time
}

// Tracker keeps the state of many origins keyed by route (or lease) ID. It is safe for concurrent
// use. OnChange is called without the tracker's lock held.
type Tracker struct {
	mu       sync.Mutex
	origins  map[string]*origin
	now      func() time.Time
	onChange func(Event)
	repeat   time.Duration
}

// New returns a tracker. onChange receives every change of state and a repeat of Down every
// repeat interval; repeat <= 0 selects DefaultRepeatInterval; now may be nil.
func New(onChange func(Event), repeat time.Duration, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	if repeat <= 0 {
		repeat = DefaultRepeatInterval
	}
	return &Tracker{origins: make(map[string]*origin), now: now, onChange: onChange, repeat: repeat}
}

// Observe records one dial or probe result for key: err == nil is a success.
func (t *Tracker) Observe(key string, err error) {
	if t == nil || key == "" {
		return
	}
	now := t.now()
	var event *Event
	t.mu.Lock()
	o := t.origins[key]
	if o == nil {
		o = &origin{}
		t.origins[key] = o
	}
	switch {
	case err == nil:
		o.failures, o.reason = 0, ""
		if o.state != Up {
			o.state = Up
			event = &Event{Key: key, State: Up, At: now}
			o.lastReport = now
		}
	default:
		o.failures++
		o.reason = Classify(err)
		if o.failures >= FailuresToDown {
			if o.state != Down {
				o.state = Down
				event = &Event{Key: key, State: Down, Reason: o.reason, At: now}
				o.lastReport = now
			} else if now.Sub(o.lastReport) >= t.repeat {
				event = &Event{Key: key, State: Down, Reason: o.reason, At: now, Repeat: true}
				o.lastReport = now
			}
		}
	}
	t.mu.Unlock()
	if event != nil && t.onChange != nil {
		t.onChange(*event)
	}
}

// State returns the current state and reason of key.
func (t *Tracker) State(key string) (State, Reason) {
	if t == nil {
		return Unknown, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if o := t.origins[key]; o != nil {
		return o.state, o.reason
	}
	return Unknown, ""
}

// Forget drops key, for example when its route was removed.
func (t *Tracker) Forget(key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.origins, key)
	t.mu.Unlock()
}

// ProbeTCP dials address once with a short timeout and returns the dial error.
func ProbeTCP(ctx context.Context, network, address string, timeout time.Duration) error {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Probe observes the result of probe for key every interval until ctx ends; interval <= 0
// selects DefaultProbeInterval. The first probe runs after one interval so a freshly started
// carrier does not report before it has served anything.
func (t *Tracker) Probe(ctx context.Context, key string, interval time.Duration, probe func(context.Context) error) {
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, interval)
			err := probe(probeCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			t.Observe(key, err)
		}
	}
}
