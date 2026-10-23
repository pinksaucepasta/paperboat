package controlbackoff

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// identityServer answers identity-bearing requests with status and counts every request it sees.
type identityServer struct {
	status int
	calls  int
}

func (s *identityServer) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls++
	return &http.Response{StatusCode: s.status, Body: http.NoBody}, nil
}

func identified() *http.Request {
	request, _ := http.NewRequest(http.MethodPost, "https://control.example.test/v1/x", nil)
	request.Header.Set("Authorization", "Bearer machine")
	return request
}

func newTransport(server *identityServer, clock *time.Time) *Transport {
	return &Transport{Base: server, Now: func() time.Time { return *clock }, Jitter: func() float64 { return 1 }}
}

func TestRejectedIdentityCostsOneRequestPerDelayNotOnePerLoopIteration(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	server := &identityServer{status: http.StatusUnauthorized}
	transport := newTransport(server, &clock)

	if response, err := transport.RoundTrip(identified()); err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the first rejection must reach the caller: %v", err)
	}
	// Every runtime loop retrying inside the delay costs no network request and no diagnostics event.
	for range 200 {
		_, err := transport.RoundTrip(identified())
		var quiet interface{ QuietFailure() bool }
		if !errors.Is(err, ErrIdentityRejected) || !errors.As(err, &quiet) || !quiet.QuietFailure() {
			t.Fatalf("a request was sent while waiting, or the refusal is noisy: %v", err)
		}
	}
	if server.calls != 1 {
		t.Fatalf("network requests = %d, want 1", server.calls)
	}
	// Requests that carry no identity (public key sets) are never held back.
	public, _ := http.NewRequest(http.MethodGet, "https://control.example.test/.well-known/jwks.json", nil)
	if _, err := transport.RoundTrip(public); err != nil || server.calls != 2 {
		t.Fatalf("public request held back: %v calls=%d", err, server.calls)
	}
	// After the delay one request probes, and the others still wait.
	clock = clock.Add(FirstDelay + time.Second)
	if _, err := transport.RoundTrip(identified()); err != nil || server.calls != 3 {
		t.Fatalf("no probe after the delay: %v calls=%d", err, server.calls)
	}
	if _, err := transport.RoundTrip(identified()); !errors.Is(err, ErrIdentityRejected) {
		t.Fatalf("a second request followed the probe: %v", err)
	}
	if got := transport.until.Sub(clock); got != 2*FirstDelay {
		t.Fatalf("second delay = %v, want %v", got, 2*FirstDelay)
	}
}

func TestDelayGrowsToTheCapAndASuccessClosesTheBreaker(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	server := &identityServer{status: http.StatusUnauthorized}
	transport := newTransport(server, &clock)
	for range 12 {
		clock = clock.Add(2 * MaximumDelay)
		_, _ = transport.RoundTrip(identified())
	}
	if got := transport.until.Sub(clock); got != MaximumDelay {
		t.Fatalf("delay = %v, want the cap %v", got, MaximumDelay)
	}
	server.status = http.StatusOK
	clock = clock.Add(2 * MaximumDelay)
	if _, err := transport.RoundTrip(identified()); err != nil {
		t.Fatal(err)
	}
	before := server.calls
	for range 5 {
		if _, err := transport.RoundTrip(identified()); err != nil {
			t.Fatalf("breaker did not close after a success: %v", err)
		}
	}
	if server.calls != before+5 {
		t.Fatalf("requests held back after recovery: %d of 5 sent", server.calls-before)
	}
}

func TestANetworkFailureDoesNotCountAsARejection(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	transport := &Transport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }), Now: func() time.Time { return clock }}
	for range 5 {
		if _, err := transport.RoundTrip(identified()); err == nil || errors.Is(err, ErrIdentityRejected) {
			t.Fatalf("network failure treated as a rejected identity: %v", err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestOnlyTheControlHostIsTrackedWhenOneIsGiven(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	server := &identityServer{status: http.StatusUnauthorized}
	transport := newTransport(server, &clock)
	transport.Host = "control.example.test"
	relay, _ := http.NewRequest(http.MethodGet, "https://relay.example.test/x", nil)
	relay.Header.Set("Authorization", "Bearer grant")
	for range 3 {
		if _, err := transport.RoundTrip(relay); err != nil {
			t.Fatalf("another server's rejection held a request back: %v", err)
		}
	}
	if _, err := transport.RoundTrip(identified()); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(identified()); !errors.Is(err, ErrIdentityRejected) {
		t.Fatalf("the control host was not held back: %v", err)
	}
	if _, err := transport.RoundTrip(relay); err != nil {
		t.Fatalf("the relay request was held back by the control plane's rejection: %v", err)
	}
}

// A new sign-in after the previous session was revoked (pb login, pb login --api-token-file) brings
// a different credential: the rejection of the old one must not hold the new one back, or the
// daemon's refresh right after the sign-in fails (F18-06 on hp).
func TestARejectedCredentialDoesNotHoldBackANewOne(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	server := &identityServer{status: http.StatusUnauthorized}
	transport := newTransport(server, &clock)
	if _, err := transport.RoundTrip(identified()); err != nil {
		t.Fatal(err)
	}
	server.status = http.StatusOK
	fresh := identified()
	fresh.Header.Set("Authorization", "Bearer new-session")
	if response, err := transport.RoundTrip(fresh); err != nil || response.StatusCode != http.StatusOK || server.calls != 2 {
		t.Fatalf("the new credential was held back by the old one's rejection: %v calls=%d", err, server.calls)
	}
	if _, err := transport.RoundTrip(identified()); !errors.Is(err, ErrIdentityRejected) {
		t.Fatalf("the rejected credential is no longer held back: %v", err)
	}
}
