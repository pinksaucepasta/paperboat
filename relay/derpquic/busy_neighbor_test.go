package derpquic

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

// PR-01: an account that exhausts its byte budget loses its own carriers (ErrOverload) and nothing else: another
// account on the same relay keeps forwarding both ways, and a flood from the busy account does not delay its round trips.
func TestBusyAccountDoesNotSlowANeighbor(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	pair := func(account, name string) (*Client, *Client, key.NodePublic, key.NodePublic, Grant) {
		ka, kb := key.NewNode().Public(), key.NewNode().Public()
		ca, cb := certificate(t), certificate(t)
		ga, gb := f.grant(ka, ca, name+"-a"), f.grant(kb, cb, name+"-b")
		ga.AccountID, gb.AccountID = account, account
		pairScopes(&ga, &gb)
		a, b := f.client(address, ca, ga), f.client(address, cb, gb)
		connect(t, a)
		connect(t, b)
		return a, b, ka, kb, ga
	}
	busyA, busyB, _, busyKeyB, busyGrant := pair("busy", "busy")
	quietA, quietB, quietKeyA, quietKeyB, _ := pair("quiet", "quiet")
	data := make([]byte, 1312)
	data[0] = 4

	// Flood packets and the packets sent after the budget ran out differ in one byte, so a leaked one is told apart
	// from a flood packet still in flight when the budget ran out.
	over := append([]byte(nil), data...)
	over[1] = 1
	var leaked atomic.Int64
	go func() {
		for {
			m, _, err := busyB.RecvDetail()
			if err != nil {
				return
			}
			if p, ok := m.(derp.ReceivedPacket); ok && bytes.Equal(p.Data, over) {
				leaked.Add(1)
			}
		}
	}()
	// Neighbor round trips while the busy account floods its carrier at full speed.
	stop := make(chan struct{})
	var flood sync.WaitGroup
	flood.Add(1)
	go func() {
		defer flood.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if busyA.Send(busyKeyB, data) != nil {
				return
			}
		}
	}()
	var worst time.Duration
	for range 50 {
		start := time.Now()
		if err := quietA.Send(quietKeyB, data); err != nil {
			t.Fatal(err)
		}
		receive(t, quietB, quietKeyA, data)
		worst = max(worst, time.Since(start))
	}
	close(stop)
	flood.Wait()
	if worst > time.Second {
		t.Fatalf("a neighbor flooding its carrier delayed a quiet account's packet by %v", worst)
	}

	// The busy account's budget runs out: its carriers are refused, the neighbor is untouched.
	s.mu.Lock()
	budget := s.accounts[busyGrant.AccountID]
	s.mu.Unlock()
	if budget == nil {
		t.Fatal("busy account has no budget")
	}
	budget.mu.Lock()
	budget.tokens, budget.at = 0, time.Now().Add(time.Minute) // no refill: the account is out of bytes
	budget.mu.Unlock()
	// Data over the exhausted budget is dropped and counted, not forwarded; the carrier itself stays up.
	dropped := s.dropped.Load()
	for range 20 {
		if err := busyA.Send(busyKeyB, over); err != nil {
			t.Fatal(err)
		}
	}
	// The relay reads a carrier's datagrams in order and drops each over-budget one at once, so its drop count has
	// stopped moving only after it has decided all 20 (and the flood's last packets before them).
	eventually(t, func() bool { return s.dropped.Load() >= dropped+20 })
	stableFor(t, time.Second, func() uint64 { return s.dropped.Load() })
	if leaked.Load() != 0 {
		t.Fatal("a packet over the account budget was forwarded")
	}
	if err := quietA.Send(quietKeyB, data); err != nil {
		t.Fatal(err)
	}
	receive(t, quietB, quietKeyA, data)
	if err := quietB.Send(quietKeyA, data); err != nil {
		t.Fatal(err)
	}
	receive(t, quietA, quietKeyB, data)
}

// stableFor returns once value has not changed for the whole of window; a value still moving keeps it waiting.
func stableFor(t testing.TB, window time.Duration, value func() uint64) {
	t.Helper()
	last, since := value(), time.Now()
	eventually(t, func() bool {
		if v := value(); v != last {
			last, since = v, time.Now()
		}
		return time.Since(since) >= window
	})
}
