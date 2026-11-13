package edgehttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAddressKeyGroupsIPv6By64(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":              "192.0.2.7",
		"::ffff:192.0.2.7":       "192.0.2.7",
		"2001:db8:1:2:aaaa::1":   "2001:db8:1:2::",
		"2001:db8:1:2:bbbb::9":   "2001:db8:1:2::",
		"not-an-address:private": "not-an-address:private",
	} {
		if got := addressKey(in); got != want {
			t.Fatalf("addressKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// One address is held to its rate and in-flight bound; other addresses, as many as a popular link
// brings, are untouched.
func TestAddressLimiterStopsOneAddressNotTheCrowd(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newAddressLimiter(addressRate{perSecond: 10, burst: 20}, addressRate{perSecond: 1, burst: 2}, 5)
	l.now = func() time.Time { return now }
	var releases []func()
	for i := 0; i < 5; i++ {
		release, ok := l.admit("192.0.2.1", false)
		if !ok {
			t.Fatalf("request %d refused", i)
		}
		releases = append(releases, release)
	}
	if _, ok := l.admit("192.0.2.1", false); ok {
		t.Fatal("sixth in-flight request admitted")
	}
	releases[0]()
	releases[0]() // a repeated release frees one slot only
	if release, ok := l.admit("192.0.2.1", false); !ok {
		t.Fatal("freed slot not reused")
	} else {
		release()
	}
	if _, ok := l.admit("192.0.2.1", false); !ok {
		t.Fatal("second free slot missing")
	}
	// Rate: 20 burst, 7 taken; 13 more pass (with releases), then refusal until refill.
	for _, release := range releases[1:] {
		release()
	}
	l.streams = 0
	passed := 0
	for i := 0; i < 30; i++ {
		if _, ok := l.admit("192.0.2.1", false); ok {
			passed++
		}
	}
	if passed != 13 {
		t.Fatalf("passed %d past the burst, want 13", passed)
	}
	now = now.Add(time.Second)
	if _, ok := l.admit("192.0.2.1", false); !ok {
		t.Fatal("no refill after a second")
	}
	// Upgrades have their own, smaller bucket; a refused upgrade does not spend a request token.
	if _, ok := l.admit("192.0.2.2", true); !ok {
		t.Fatal("first upgrade refused")
	}
	if _, ok := l.admit("192.0.2.2", true); !ok {
		t.Fatal("second upgrade refused")
	}
	if _, ok := l.admit("192.0.2.2", true); ok {
		t.Fatal("upgrade past its burst admitted")
	}
	if l.shards[0].keys == nil {
		t.Fatal("shards not initialised")
	}
	for i := 0; i < 1000; i++ {
		if _, ok := l.admit(fmt.Sprintf("198.51.%d.%d", i/250, i%250), false); !ok {
			t.Fatalf("crowd visitor %d refused", i)
		}
	}
}

func TestAddressLimiterExpiresIdleAddresses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newAddressLimiter(addressRequestRate, addressUpgradeRate, addressStreams)
	l.now = func() time.Time { return now }
	held, _ := l.admit("192.0.2.1", false)
	idle, _ := l.admit("192.0.2.2", false)
	idle()
	now = now.Add(addressIdle + addressSweepPeriod)
	l.admit("192.0.2.3", false) // a new key in each shard sweeps it; sweep every shard here
	for i := range l.shards {
		l.shards[i].mu.Lock()
		l.shards[i].sweep(now)
		l.shards[i].mu.Unlock()
	}
	total := 0
	for i := range l.shards {
		total += len(l.shards[i].keys)
	}
	if total != 2 {
		t.Fatalf("%d keys after sweep, want the held one and the new one", total)
	}
	held()
}

// Over the address limit, the edge answers 429 edge_rate_limited with Retry-After before any route
// or origin work.
func TestPolicyRefusesAnAddressOverItsLimit(t *testing.T) {
	policy := &Policy{addresses: newAddressLimiter(addressRate{perSecond: 0.001, burst: 1}, addressUpgradeRate, addressStreams)}
	request := func() (*httptest.ResponseRecorder, bool) {
		recorder := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "https://app.example.test/", nil)
		r.RemoteAddr = "192.0.2.9:4444"
		release, ok := policy.admitAddress(recorder, r)
		if ok {
			release()
		}
		return recorder, ok
	}
	if _, ok := request(); !ok {
		t.Fatal("first request refused")
	}
	second, ok := request()
	if ok || second.Code != http.StatusTooManyRequests || second.Header().Get("X-Paperboat-Error") != "edge_rate_limited" || second.Header().Get("Retry-After") == "" {
		t.Fatalf("limited request: ok=%v %d %q %q", ok, second.Code, second.Header().Get("X-Paperboat-Error"), second.Header().Get("Retry-After"))
	}
}
