package derpquic

import (
	"context"
	"errors"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// A relay that left an account's pool keeps its carriers until the handover deadline (make-before-break), then
// closes them and refuses the account with the non-fatal ErrOverload so clients reconnect through the new relays;
// accounts not listed are untouched and an empty list ends the refusal.
func TestPoolHandoverKeepsCarriersUntilTheDeadlineThenClosesThemNonFatally(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	quic := f.client(address, ca, ga)
	wss, _ := f.wss(s, cb, gb, nil)
	connect(t, quic)
	connectWSS(t, wss)

	deadline := time.Now().Add(400 * time.Millisecond)
	if err := s.SetPoolHandover([]Handover{{AccountID: "account", CloseAt: deadline}}); err != nil {
		t.Fatal(err)
	}
	// Before the deadline the account is served as before, new admissions included.
	if quic.current.conn.Context().Err() != nil {
		t.Fatal("handover closed a carrier before its deadline")
	}
	early := f.client(address, ca, ga)
	connect(t, early)

	eventually(t, func() bool { quic.mu.Lock(); defer quic.mu.Unlock(); return quic.current.conn.Context().Err() != nil })
	eventually(t, func() bool {
		wss.mu.Lock()
		defer wss.mu.Unlock()
		return wss.current != nil && wss.current.ctx.Err() != nil
	})
	if time.Now().Before(deadline) {
		t.Fatal("carriers closed before the handover deadline")
	}
	late := f.client(address, ca, ga)
	var refused *Error
	if err := late.Connect(context.Background()); !errors.Is(err, ErrOverload) || !errors.As(err, &refused) || refused.Fatal() {
		t.Fatalf("admission after the deadline: %v, want the non-fatal ErrOverload", err)
	}

	if err := s.SetPoolHandover([]Handover{{AccountID: "someone-else", CloseAt: time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	connect(t, f.client(address, ca, ga))
	if err := s.SetPoolHandover(nil); err != nil {
		t.Fatal(err)
	}
	connect(t, f.client(address, ca, ga))
}

func TestSetPoolHandoverRefusesAnEntryWithoutAnAccount(t *testing.T) {
	f := newFixture(t)
	s, _, _ := f.start("127.0.0.1:0")
	if err := s.SetPoolHandover([]Handover{{AccountID: ""}}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("empty account: %v", err)
	}
}
