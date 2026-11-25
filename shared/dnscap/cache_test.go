package dnscap

import (
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
	set := func(age time.Duration) {
		mu.Lock()
		hosts["a.example.test"] = entry{Address: netip.MustParseAddr("203.0.113.1"), Verified: time.Now().Add(-age)}
		mu.Unlock()
	}
	if _, fresh := Cached("a.example.test"); !fresh {
		t.Fatal("just verified is not fresh")
	}
	set(11 * time.Minute)
	if a, fresh := Cached("a.example.test"); fresh || !a.IsValid() {
		t.Fatal("an 11 minute old address must be last good, not fresh")
	}
	for _, age := range []time.Duration{25 * time.Hour, -time.Hour} {
		set(age)
		if a, _ := Cached("a.example.test"); a.IsValid() {
			t.Fatalf("an address verified %s ago is still used", age)
		}
	}
	set(0)
	Expire("a.example.test", netip.MustParseAddr("203.0.113.2"))
	if _, fresh := Cached("a.example.test"); !fresh {
		t.Fatal("the failure of another address expired this one")
	}
	Expire("a.example.test", netip.MustParseAddr("203.0.113.1"))
	if a, fresh := Cached("a.example.test"); fresh || !a.IsValid() {
		t.Fatal("a failed address must be last good, not fresh")
	}
}

func TestPrivateAddressesAndLiteralsAreNotKept(t *testing.T) {
	reset(t)
	for host, ip := range map[string]string{"a.test": "10.0.0.1", "b.test": "127.0.0.1", "c.test": "fe80::1", "d.test": "fd00::1", "203.0.113.1": "203.0.113.1"} {
		Verified(host, tcp(ip))
		if a, _ := Cached(host); a.IsValid() {
			t.Errorf("%s at %s was kept", host, ip)
		}
	}
	Persist([]byte(`{"e.test":{"address":"10.0.0.1","verified":"`+time.Now().UTC().Format(time.RFC3339)+`"}}`), nil)
	if a, _ := Cached("e.test"); a.IsValid() {
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
			Expire(host, netip.MustParseAddr("203.0.113.1"))
		})
	}
	wg.Wait()
	if len(hosts) > maxHosts {
		t.Fatalf("%d hosts kept", len(hosts))
	}
}
