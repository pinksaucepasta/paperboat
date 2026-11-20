package addrlimit

import (
	"fmt"
	"testing"
	"time"
)

func TestKeyGroupsIPv6By64(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":              "192.0.2.7",
		"::ffff:192.0.2.7":       "192.0.2.7",
		"2001:db8:1:2:aaaa::1":   "2001:db8:1:2::",
		"2001:db8:1:2:bbbb::9":   "2001:db8:1:2::",
		"not-an-address:private": "not-an-address:private",
	} {
		if got := Key(in); got != want {
			t.Fatalf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

// One address is held to its rate and in-flight bound; other addresses, as many as a popular link
// brings, are untouched.
func TestAddressLimiterStopsOneAddressNotTheCrowd(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(Rate{PerSecond: 10, Burst: 20}, Rate{PerSecond: 1, Burst: 2}, 5)
	l.SetClock(func() time.Time { return now })
	var releases []func()
	for i := 0; i < 5; i++ {
		release, ok := l.Admit("192.0.2.1", false)
		if !ok {
			t.Fatalf("request %d refused", i)
		}
		releases = append(releases, release)
	}
	if _, ok := l.Admit("192.0.2.1", false); ok {
		t.Fatal("sixth in-flight request admitted")
	}
	releases[0]()
	releases[0]() // a repeated release frees one slot only
	if release, ok := l.Admit("192.0.2.1", false); !ok {
		t.Fatal("freed slot not reused")
	} else {
		release()
	}
	if _, ok := l.Admit("192.0.2.1", false); !ok {
		t.Fatal("second free slot missing")
	}
	// Rate: 20 burst, 7 taken; 13 more pass (with releases), then refusal until refill.
	for _, release := range releases[1:] {
		release()
	}
	l.streams = 0
	passed := 0
	for i := 0; i < 30; i++ {
		if _, ok := l.Admit("192.0.2.1", false); ok {
			passed++
		}
	}
	if passed != 13 {
		t.Fatalf("passed %d past the burst, want 13", passed)
	}
	now = now.Add(time.Second)
	if _, ok := l.Admit("192.0.2.1", false); !ok {
		t.Fatal("no refill after a second")
	}
	// Upgrades have their own, smaller bucket; a refused upgrade does not spend a request token.
	if _, ok := l.Admit("192.0.2.2", true); !ok {
		t.Fatal("first upgrade refused")
	}
	if _, ok := l.Admit("192.0.2.2", true); !ok {
		t.Fatal("second upgrade refused")
	}
	if _, ok := l.Admit("192.0.2.2", true); ok {
		t.Fatal("upgrade past its burst admitted")
	}
	if l.shards[0].keys == nil {
		t.Fatal("shards not initialised")
	}
	for i := 0; i < 1000; i++ {
		if _, ok := l.Admit(fmt.Sprintf("198.51.%d.%d", i/250, i%250), false); !ok {
			t.Fatalf("crowd visitor %d refused", i)
		}
	}
}

func TestAddressLimiterExpiresIdleAddresses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(Rate{PerSecond: 100, Burst: 3000}, Rate{PerSecond: 20, Burst: 40}, Streams)
	l.SetClock(func() time.Time { return now })
	held, _ := l.Admit("192.0.2.1", false)
	gone, _ := l.Admit("192.0.2.2", false)
	gone()
	now = now.Add(idle + sweepPeriod)
	l.Admit("192.0.2.3", false) // a new key in each shard sweeps it; sweep every shard here
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

// Memory stays bounded: a shard holds at most its key limit however many addresses arrive, and idle ones are swept.
func TestLimiterKeepsMemoryBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(Rate{PerSecond: 100, Burst: 3000}, Rate{}, 0)
	l.SetClock(func() time.Time { return now })
	l.shardKeys = 8
	for i := 0; i < 100_000; i++ {
		if _, ok := l.Admit(fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&255, i&255), false); !ok {
			t.Fatalf("visitor %d refused", i)
		}
	}
	count := func() int {
		total := 0
		for i := range l.shards {
			l.shards[i].mu.Lock()
			total += len(l.shards[i].keys)
			l.shards[i].mu.Unlock()
		}
		return total
	}
	if total := count(); total > 8*shardCount {
		t.Fatalf("%d addresses tracked, bound is %d", total, 8*shardCount)
	}
	now = now.Add(idle + sweepPeriod)
	for i := range l.shards {
		l.shards[i].mu.Lock()
		l.shards[i].sweep(now)
		l.shards[i].mu.Unlock()
	}
	if total := count(); total != 0 {
		t.Fatalf("%d addresses tracked after the idle sweep", total)
	}
}

// NewInFlight bounds only what one address holds at once, never its rate.
func TestInFlightLimiterBoundsHoldsNotRate(t *testing.T) {
	l := NewInFlight(3)
	var releases []func()
	for i := 0; i < 3; i++ {
		release, ok := l.Admit("192.0.2.1", false)
		if !ok {
			t.Fatalf("hold %d refused", i)
		}
		releases = append(releases, release)
	}
	if _, ok := l.Admit("192.0.2.1", false); ok {
		t.Fatal("fourth hold admitted")
	}
	if _, ok := l.Admit("192.0.2.2", false); !ok {
		t.Fatal("another address refused")
	}
	releases[0]()
	for i := 0; i < 1000; i++ { // churn: a rate limit would refuse long before this
		release, ok := l.Admit("192.0.2.1", false)
		if !ok {
			t.Fatalf("churn %d refused", i)
		}
		release()
	}
}
