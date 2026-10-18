package derpquic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"tailscale.com/types/key"
)

// F13-03 relay admission: accounts the control plane lists as out of bandwidth are refused with
// the typed, non-fatal ErrQuota (new admissions and live carriers, QUIC and WSS), unlisted
// accounts are untouched, and the refusal ends the moment the account is no longer listed.
func TestRelayRefusesQuotaDeniedAccountsAndRecovers(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	quic := f.client(address, ca, ga)
	wss, _ := f.wss(s, cb, gb, nil)
	connect(t, quic)
	connectWSS(t, wss)

	if err := s.SetQuotaDenied([]string{"account"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { quic.mu.Lock(); defer quic.mu.Unlock(); return quic.current.conn.Context().Err() != nil })
	eventually(t, func() bool {
		wss.mu.Lock()
		defer wss.mu.Unlock()
		return wss.current != nil && wss.current.ctx.Err() != nil
	})

	newQUIC := f.client(address, ca, ga)
	if err := newQUIC.Connect(context.Background()); !errors.Is(err, ErrQuota) {
		t.Fatalf("QUIC admission of a quota-denied account: %v, want ErrQuota", err)
	}
	newWSS, _ := f.wss(s, cb, gb, nil)
	if err := newWSS.Connect(context.Background()); !errors.Is(err, ErrQuota) {
		t.Fatalf("WSS admission of a quota-denied account: %v, want ErrQuota", err)
	}
	var refused *Error
	if err := newQUIC.Connect(context.Background()); !errors.As(err, &refused) || refused.Fatal() {
		t.Fatalf("a quota refusal must be non-fatal so clients back off instead of giving up: %v", err)
	}

	if err := s.SetQuotaDenied([]string{"someone-else"}); err != nil {
		t.Fatal(err)
	}
	other := f.client(address, ca, ga)
	connect(t, other)
	if err := s.SetQuotaDenied(nil); err != nil {
		t.Fatal(err)
	}
	again := f.client(address, ca, ga)
	connect(t, again)
	againWSS, _ := f.wss(s, cb, gb, nil)
	connectWSS(t, againWSS)
}

func TestSetQuotaDeniedIsBounded(t *testing.T) {
	f := newFixture(t)
	s, _, _ := f.start("127.0.0.1:0")
	if err := s.SetQuotaDenied(make([]string, MaxConnections+1)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("oversized list: %v", err)
	}
	if err := s.SetQuotaDenied([]string{""}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("empty account: %v", err)
	}
	if err := s.SetQuotaDenied([]string{strings.Repeat("a", 257)}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("oversized account id: %v", err)
	}
	if err := s.SetQuotaDenied(nil); err != nil {
		t.Fatalf("clearing: %v", err)
	}
}
