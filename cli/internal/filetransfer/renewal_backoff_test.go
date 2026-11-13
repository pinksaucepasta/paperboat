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
	client.renewDelay = func(n int) time.Duration { delays = append(delays, n); return 0 }
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
		t.Fatalf("renewals = %d, want %d", renewed, maxRenewalsWithoutProgress)
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
