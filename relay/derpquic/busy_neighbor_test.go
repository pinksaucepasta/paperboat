package derpquic

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
	_ = busyB

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
	_ = busyA.Send(busyKeyB, data) // may already be closed by the flood; either way the carrier must end
	busyA.mu.Lock()
	cc := busyA.current
	busyA.mu.Unlock()
	select {
	case <-cc.conn.Context().Done():
		if err := classify(context.Cause(cc.conn.Context())); !errors.Is(err, ErrOverload) {
			t.Fatalf("budget-exhausted carrier closed with %v, want overload", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("budget-exhausted carrier stayed open")
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
