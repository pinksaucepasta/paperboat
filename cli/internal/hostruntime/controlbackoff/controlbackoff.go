// Package controlbackoff keeps a computer whose machine identity the control plane rejects (it was
// removed from the account) from hammering the control plane. Every runtime loop shares one
// transport; once an identity-bearing request is answered 401, further requests with that same
// identity fail locally and quietly until a growing, jittered delay has passed, when exactly one
// request probes whether the identity works again. Requests without an identity (public key sets,
// say) or with another one (the credential of a new sign-in) are never held back, and a success of
// the rejected identity closes the breaker.
package controlbackoff

import (
	"crypto/sha256"
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/serverclock"
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

	mu       sync.Mutex
	rejected [sha256.Size]byte // the identity the streak is about
	streak   int
	until    time.Time
	probing  bool
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

// identity names the identity a request authenticates with: the machine (its proofs differ per
// request but name one identity) or a bearer credential. ok is false for requests without one.
func (t *Transport) identity(request *http.Request) (key [sha256.Size]byte, ok bool) {
	if t.Host != "" && request.URL.Host != t.Host {
		return key, false
	}
	if request.Header.Get("X-Paperboat-Machine-Proof") != "" {
		return sha256.Sum256([]byte("machine")), true
	}
	if bearer := request.Header.Get("Authorization"); bearer != "" {
		return sha256.Sum256([]byte("bearer:" + bearer)), true
	}
	return key, false
}

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	key, ok := t.identity(request)
	if !ok {
		return t.Base.RoundTrip(request)
	}
	probe, err := t.admit(key)
	if err != nil {
		return nil, err
	}
	response, err := t.Base.RoundTrip(request)
	t.observe(key, probe, response, err)
	return response, err
}

// admit lets a request through, or refuses it while the delay runs. After the delay one request at
// a time probes.
func (t *Transport) admit(key [sha256.Size]byte) (probe bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.streak == 0 || key != t.rejected {
		return false, nil
	}
	if t.now().Before(t.until) || t.probing {
		return false, rejectedError{retryAt: t.until}
	}
	t.probing = true
	return true, nil
}

func (t *Transport) observe(key [sha256.Size]byte, probe bool, response *http.Response, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if probe {
		t.probing = false
	}
	switch {
	case err != nil:
		// A network failure says nothing about the identity.
	case response.StatusCode == http.StatusUnauthorized && clockSkewed(response, t.now()):
		// A proof signed with a clock a minute or more off is refused although the identity is good;
		// serverclock corrects the next proof from this answer's Date, so the identity is not held back.
	case response.StatusCode == http.StatusUnauthorized:
		if key != t.rejected {
			t.rejected, t.streak, t.probing = key, 0, false
		}
		t.streak++
		delay := FirstDelay << min(t.streak-1, 8)
		if delay > MaximumDelay {
			delay = MaximumDelay
		}
		t.until = t.now().Add(time.Duration(float64(delay) * t.jitter()))
	case response.StatusCode < http.StatusBadRequest && key == t.rejected:
		t.streak = 0
	}
}

// clockSkewed reports whether the server's Date is at least serverclock.Threshold from now (local).
func clockSkewed(response *http.Response, now time.Time) bool {
	server, err := http.ParseTime(response.Header.Get("Date"))
	return err == nil && server.Sub(now).Abs() >= serverclock.Threshold
}
