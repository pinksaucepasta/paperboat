package quicgate

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/quic-go/quic-go"
)

func addr(host string) net.Addr { return &net.UDPAddr{IP: net.ParseIP(host), Port: 4433} }

func admit(t *testing.T, tr *quic.Transport, host string) (context.Context, context.CancelFunc, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ctx, err := tr.ConnContext(ctx, &quic.ClientInfo{RemoteAddr: addr(host)})
	return ctx, cancel, err
}

// Visitors: a fresh address gets no Retry; once it has an unfinished handshake further ones must prove the address, and
// past the per-address bound they are refused; finished handshakes free their slot and other addresses are untouched.
func TestVisitorGateRetriesOnlyAnAddressWithPendingHandshakes(t *testing.T) {
	const bound = 3
	tr := NewTransport(nil, bound, false)
	if tr.VerifySourceAddress(addr("203.0.113.7")) {
		t.Fatal("a fresh address was forced through a Retry round trip")
	}
	var contexts []context.Context
	for i := range bound {
		ctx, cancel, err := admit(t, tr, "203.0.113.7")
		if err != nil {
			t.Fatalf("handshake %d refused: %v", i+1, err)
		}
		defer cancel()
		contexts = append(contexts, ctx)
		if !tr.VerifySourceAddress(addr("203.0.113.7")) {
			t.Fatalf("an address with %d unfinished handshakes was not asked to prove itself", i+1)
		}
	}
	if tr.VerifySourceAddress(addr("203.0.113.8")) {
		t.Fatal("another address was asked to prove itself")
	}
	if _, cancel, err := admit(t, tr, "203.0.113.7"); !errors.Is(err, ErrPending) {
		cancel()
		t.Fatalf("handshake past the bound = %v, want ErrPending", err)
	}
	if _, cancel, err := admit(t, tr, "203.0.113.8"); err != nil {
		t.Fatalf("another address refused: %v", err)
	} else {
		cancel()
	}
	Settle(contexts[0]) // a finished handshake frees its slot
	if ctx, cancel, err := admit(t, tr, "203.0.113.7"); err != nil {
		t.Fatalf("slot not freed after the handshake finished: %v", err)
	} else {
		defer cancel()
		_ = ctx
	}
	// An IPv6 visitor is bounded by its /64, not its single address.
	for i := range bound {
		_, cancel, err := admit(t, tr, "2001:db8::"+string(rune('1'+i)))
		if err != nil {
			t.Fatalf("v6 handshake %d: %v", i+1, err)
		}
		defer cancel()
	}
	if _, cancel, err := admit(t, tr, "2001:db8::ff"); !errors.Is(err, ErrPending) {
		cancel()
		t.Fatalf("a fourth address of one /64 = %v, want ErrPending", err)
	}
}

// Machine carriers: every address proves itself first.
func TestCarrierGateRetriesEveryAddress(t *testing.T) {
	tr := NewTransport(nil, 3, true)
	if !tr.VerifySourceAddress(addr("203.0.113.7")) {
		t.Fatal("a carrier address skipped the Retry")
	}
}
