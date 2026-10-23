package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestBurstThenRefillPerKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := New(60, 3, 100)
	l.now = func() time.Time { return now }
	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("over the burst: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another key is limited too")
	}
	now = now.Add(time.Second) // 60/min refills one per second
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("not refilled")
	}
	if ok, _ := l.Check("a"); ok {
		t.Fatal("Check after the refill was taken should say no")
	}
}

func TestMemoryIsBounded(t *testing.T) {
	l := New(60, 3, 10)
	for i := range 1000 {
		l.Allow(fmt.Sprint("ip-", i))
	}
	if len(l.buckets) > 10 {
		t.Fatalf("%d keys kept", len(l.buckets))
	}
}

func TestZeroConfigurationIsOff(t *testing.T) {
	l := New(0, 0, 0)
	for range 100 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatal("a zero limiter refused")
		}
	}
}
