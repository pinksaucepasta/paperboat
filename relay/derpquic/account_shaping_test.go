package derpquic

import (
	"testing"
	"time"

	"tailscale.com/types/key"
)

// Over its speed an account is shaped, not policed: packets wait for their tokens up to shapingMaxDelay and are dropped only past it, the
// delays grow at the account's rate, and offered at twice the speed the forwarded bytes still track the speed (so the account's TCP
// settles at it) instead of collapsing under drops.
func TestBudgetReserveShapesOverCapTrafficInsteadOfDropping(t *testing.T) {
	const rate = 64_000.0 // 512 kbit/s
	b := newBudgetBurst(rate, accountBurst(rate))
	now := time.Now()
	// The burst (a quarter second) passes at once; the next packets wait in turn, each 1400/rate longer.
	var previous time.Duration
	immediate, delayed := 0, 0
	for range 40 {
		delay, ok := b.reserve(now, 1400, shapingMaxDelay)
		if !ok {
			break
		}
		if delay == 0 {
			immediate++
		} else {
			delayed++
			if delay <= previous {
				t.Fatalf("delay %v did not grow past %v", delay, previous)
			}
			previous = delay
		}
	}
	if immediate < 10 || immediate > 12 || delayed < 20 || previous > shapingMaxDelay {
		t.Fatalf("burst %d packets at once then %d delayed (last wait %v), want ~11 then ~22 up to %v", immediate, delayed, previous, shapingMaxDelay)
	}
	if _, ok := b.reserve(now, 1400, shapingMaxDelay); ok {
		t.Fatal("a packet that would wait past the buffer bound was queued")
	}
	// Offered at twice the speed for 20 s (a sender that keeps sending, packets spaced by 1400 B at 2*rate), what is forwarded
	// (queued and released at its time) tracks the speed: no collapse below it, and nothing above it beyond burst plus buffer.
	b = newBudgetBurst(rate, accountBurst(rate))
	start := time.Now()
	forwarded, dropped := 0, 0
	gap := time.Duration(1400 / (2 * rate) * float64(time.Second))
	for at := start; at.Before(start.Add(20 * time.Second)); at = at.Add(gap) {
		if _, ok := b.reserve(at, 1400, shapingMaxDelay); ok {
			forwarded += 1400
		} else {
			dropped++
		}
	}
	most := int(rate*20) + int(accountBurst(rate)) + int(rate*shapingMaxDelay.Seconds()) + 1400
	if forwarded > most || forwarded < int(rate*20)*95/100 {
		t.Fatalf("offered at 2x for 20 s the account forwarded %d bytes (dropped %d packets), want between %d and %d", forwarded, dropped, int(rate*20)*95/100, most)
	}
}

// Through a relay, a burst over the account's speed arrives complete, in order and paced, where policing would have dropped most of it:
// 40 packets (52 KB) at 1 Mbit/s with a quarter-second burst take about 0.17 s longer than the burst alone, and none is lost.
func TestRelayShapesAnAccountsBurstInsteadOfDroppingIt(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	if err := s.SetAccountSpeed(1); err != nil { // 1 Mbit/s: 125 kB/s, 31 kB burst
		t.Fatal(err)
	}
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "shape-a"), f.grant(kb, cb, "shape-b")
	ga.AccountID, gb.AccountID = "shaped", "shaped"
	pairScopes(&ga, &gb)
	a, b := f.client(address, ca, ga), f.client(address, cb, gb)
	connect(t, a)
	connect(t, b)
	const packets = 40
	started := time.Now()
	go func() {
		for i := range packets {
			data := make([]byte, 1312)
			data[0], data[1] = 4, byte(i)
			if a.Send(kb, data) != nil {
				return
			}
		}
	}()
	for i := range packets {
		data := make([]byte, 1312)
		data[0], data[1] = 4, byte(i)
		receive(t, b, ka, data) // fails on a missing, reordered or altered packet
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("52 KB over a 1 Mbit/s account arrived in %v: not paced", elapsed)
	}
	if dropped := s.Snapshot().Dropped; dropped != 0 {
		t.Fatalf("%d packets were dropped from a burst the buffer could hold", dropped)
	}
}
