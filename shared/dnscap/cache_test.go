package dnscap

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func tcp(ip string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: 443} }

func reset(t *testing.T) {
	t.Helper()
	mu.Lock()
	hosts, save = map[string]entry{}, nil
	mu.Unlock()
}

func TestCacheAges(t *testing.T) {
	reset(t)
	Verified("A.example.test", tcp("203.0.113.1"))
	ran := make(chan struct{}, 4)
	at := func(age time.Duration) (kept, refreshed bool) {
		mu.Lock()
		hosts["a.example.test"] = entry{Address: netip.MustParseAddr("203.0.113.1"), Verified: time.Now().Add(-age)}
		mu.Unlock()
		for range 2 { // the second is inside retryAfter
			Refresh("a.example.test", func(context.Context) { ran <- struct{}{} })
		}
		select {
		case <-ran:
			refreshed = true
		case <-time.After(50 * time.Millisecond):
		}
		if len(ran) != 0 {
			t.Fatal("two refreshes at once")
		}
		return Cached("a.example.test").IsValid(), refreshed
	}
	for age, want := range map[time.Duration][2]bool{0: {true, false}, 11 * time.Minute: {true, true}, 23 * time.Hour: {true, true}} {
		if kept, refreshed := at(age); kept != want[0] || refreshed != want[1] {
			t.Errorf("%s old: kept %v refreshed %v, want %v", age, kept, refreshed, want)
		}
	}
	for _, age := range []time.Duration{25 * time.Hour, -time.Hour} {
		if kept, _ := at(age); kept {
			t.Errorf("an address verified %s ago is still used", age)
		}
	}
}

func TestPrivateAddressesAndLiteralsAreNotKept(t *testing.T) {
	reset(t)
	for host, ip := range map[string]string{"a.test": "10.0.0.1", "b.test": "127.0.0.1", "c.test": "fe80::1", "d.test": "fd00::1", "203.0.113.1": "203.0.113.1"} {
		Verified(host, tcp(ip))
		if Cached(host).IsValid() {
			t.Errorf("%s at %s was kept", host, ip)
		}
	}
	Persist([]byte(`{"e.test":{"address":"10.0.0.1","verified":"`+time.Now().UTC().Format(time.RFC3339)+`"}}`), nil)
	if Cached("e.test").IsValid() {
		t.Error("a private address was loaded")
	}
}

func TestConcurrentUseIsBounded(t *testing.T) {
	reset(t)
	Persist(nil, func([]byte) {})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			host := string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".example.test"
			Verified(host, tcp("203.0.113.1"))
			Cached(host)
			Refresh(host, func(context.Context) {})
		})
	}
	wg.Wait()
	Verified("newest.example.test", tcp("203.0.113.2"))
	if !Cached("newest.example.test").IsValid() {
		t.Fatal("a full cache refused a new host")
	}
	if len(hosts) > maxHosts {
		t.Fatalf("%d hosts kept", len(hosts))
	}
}
