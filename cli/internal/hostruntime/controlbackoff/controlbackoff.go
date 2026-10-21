// Package controlbackoff keeps a computer whose machine identity the control plane rejects (it was
// removed from the account) from hammering the control plane. Every runtime loop shares one
// transport; once an identity-bearing request is answered 401, further identity-bearing requests
// fail locally and quietly until a growing, jittered delay has passed, when exactly one request
// probes whether the identity works again. Requests without an identity (public key sets, say) are
// never held back, and any success closes the breaker.
package controlbackoff

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

const (
	// FirstDelay is the wait after the first rejection; each further rejection doubles it.
	FirstDelay = 5 * time.Second
	// MaximumDelay caps the wait, so a restored identity is noticed within about a minute.
	MaximumDelay = time.Minute
)

// ErrIdentityRejected is returned, without any network request, while the control plane's rejection
// of this computer's identity is being waited out.
var ErrIdentityRejected = errors.New("the control plane rejected this computer's identity; waiting before asking again")

type rejectedError struct{ retryAt time.Time }

func (e rejectedError) Error() string { return ErrIdentityRejected.Error() }
func (e rejectedError) Unwrap() error { return ErrIdentityRejected }

// QuietFailure marks a refusal that is expected and already explained: it needs no diagnostics
// event of its own for every held-back request.
func (rejectedError) QuietFailure() bool { return true }

// Transport wraps the runtime's control-plane transport.
type Transport struct {
	Base http.RoundTripper
	// Host is the control plane's host (with port). Only requests to it are tracked and held back, so
	// another server's rejection (a relay refusing a grant, say) never silences the control plane.
	Host string
	// Now and Jitter exist for tests; zero values use the clock and a 0.8–1.2 random factor.
	Now    func() time.Time
	Jitter func() float64

	mu      sync.Mutex
	streak  int
	until   time.Time
	probing bool
}

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Transport) jitter() float64 {
	if t.Jitter != nil {
		return t.Jitter()
	}
	return 0.8 + 0.4*rand.Float64()
}

// carriesIdentity reports whether a request authenticates as this machine.
func (t *Transport) carriesIdentity(request *http.Request) bool {
	if t.Host != "" && request.URL.Host != t.Host {
		return false
	}
	return request.Header.Get("X-Paperboat-Machine-Proof") != "" || request.Header.Get("Authorization") != ""
}

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !t.carriesIdentity(request) {
		return t.Base.RoundTrip(request)
	}
	probe, err := t.admit()
	if err != nil {
		return nil, err
	}
	response, err := t.Base.RoundTrip(request)
	t.observe(probe, response, err)
	return response, err
}

// admit lets a request through, or refuses it while the delay runs. After the delay one request at
// a time probes.
func (t *Transport) admit() (probe bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.streak == 0 {
		return false, nil
	}
	if t.now().Before(t.until) || t.probing {
		return false, rejectedError{retryAt: t.until}
	}
	t.probing = true
	return true, nil
}

func (t *Transport) observe(probe bool, response *http.Response, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if probe {
		t.probing = false
	}
	switch {
	case err != nil:
		// A network failure says nothing about the identity.
	case response.StatusCode == http.StatusUnauthorized:
		t.streak++
		delay := FirstDelay << min(t.streak-1, 8)
		if delay > MaximumDelay {
			delay = MaximumDelay
		}
		t.until = t.now().Add(time.Duration(float64(delay) * t.jitter()))
	case response.StatusCode < http.StatusBadRequest:
		t.streak = 0
	}
}
