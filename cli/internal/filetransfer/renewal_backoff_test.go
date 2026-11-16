package filetransfer

import (
	"context"
	"io"
	"testing"
	"time"
)

// X-15: the destination refused every stream (the sender sees an EOF) and the sender renewed its credential after
// each one, opening an access session per renewal until the server's operations cap answered 429.
func TestRefusedStreamsRenewAtMostBoundedTimesThenFail(t *testing.T) {
	client := NewClient("http://example.test", Auth{Token: "t0"}, Binding{}, nil)
	client.native = true
	var delays []int
	now := time.Unix(0, 0)
	client.now = func() time.Time { return now }
	client.renewDelay = func(n int) time.Duration { delays = append(delays, n); now = now.Add(time.Minute); return 0 }
	renewed := 0
	client.RefreshAuth = func(context.Context) (Auth, error) {
		renewed++
		return Auth{Token: "t" + string(rune('0'+renewed))}, nil
	}
	var last error
	for range 3 * maxRenewalsWithoutProgress {
		last = client.afterTransportLoss(t.Context(), client.currentAuth(), io.EOF)
	}
	if renewed != maxRenewalsWithoutProgress {
		t.Fatalf("renewals = %d, want %d (each a minute apart, so past the time floor)", renewed, maxRenewalsWithoutProgress)
	}
	if retryableTransferFailure(last) {
		t.Fatalf("the send must end once renewals are exhausted, got retryable %v", last)
	}
	if len(delays) != maxRenewalsWithoutProgress-1 || delays[0] != 1 {
		t.Fatalf("the first renewal is immediate and later ones back off: delays %v", delays)
	}
}

func TestACommittedChunkRestoresTheRenewalBudget(t *testing.T) {
	client := NewClient("http://example.test", Auth{Token: "t0"}, Binding{}, nil)
	client.native = true
	client.renewDelay = func(int) time.Duration { return 0 }
	renewed := 0
	client.RefreshAuth = func(context.Context) (Auth, error) {
		renewed++
		return Auth{Token: "t" + string(rune('a'+renewed))}, nil
	}
	for range maxRenewalsWithoutProgress {
		_ = client.afterTransportLoss(t.Context(), client.currentAuth(), io.EOF)
	}
	client.renewalProgressed()
	if err := client.afterTransportLoss(t.Context(), client.currentAuth(), io.EOF); !retryableTransferFailure(err) || renewed != maxRenewalsWithoutProgress+1 {
		t.Fatalf("after progress the next loss renews again: renewed %d, err %v", renewed, err)
	}
}

func TestRenewalDelayIsBoundedAndJittered(t *testing.T) {
	for n := 1; n < 100; n++ {
		if d := renewalDelay(n); d <= 0 || d > 45*time.Second {
			t.Fatalf("renewalDelay(%d) = %v", n, d)
		}
	}
}

// X-26/X-37: a peer frozen by sleep, or a relay path that dropped, while the server still grants renewals: quick
// renewals without a committed chunk must not end the send before the time floor, then resume once a chunk commits.
func TestRenewalsWithoutProgressKeepTheSendForTheTimeFloor(t *testing.T) {
	client := NewClient("http://example.test", Auth{Token: "t0"}, Binding{}, nil)
	client.native = true
	now := time.Unix(0, 0)
	client.now = func() time.Time { return now }
	client.renewDelay = func(int) time.Duration { now = now.Add(10 * time.Second); return 0 }
	client.RefreshAuth = func(context.Context) (Auth, error) { return Auth{Token: "t" + now.String()}, nil }
	for i := 0; now.Before(time.Unix(0, 0).Add(minRenewingWithoutProgress - 10*time.Second)); i++ {
		if err := client.afterTransportLoss(t.Context(), client.currentAuth(), io.EOF); !retryableTransferFailure(err) {
			t.Fatalf("renewal %d at %v without progress ended the send: %v", i+1, now.Sub(time.Unix(0, 0)), err)
		}
	}
	for range maxRenewalsWithoutProgress {
		_ = client.afterTransportLoss(t.Context(), client.currentAuth(), io.EOF)
	}
	if err := client.afterTransportLoss(t.Context(), client.currentAuth(), io.EOF); retryableTransferFailure(err) {
		t.Fatalf("past the time floor and the count the send must end, got retryable %v", err)
	}
}
