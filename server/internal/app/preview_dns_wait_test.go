package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForAddressesNeedsStableCorrectAnswers(t *testing.T) {
	wildcard, edge := []string{"140.245.11.41"}, []string{"152.67.0.60"}
	// The provider answers with the wildcard, then flaps while the record spreads, then settles.
	answers := [][]string{wildcard, wildcard, edge, wildcard, edge, edge, edge}
	calls := 0
	lookup := func(context.Context) ([]string, error) {
		a := answers[min(calls, len(answers)-1)]
		calls++
		return a, nil
	}
	if !waitForAddresses(context.Background(), lookup, edge, time.Millisecond, time.Second) || calls != 7 {
		t.Fatalf("settled=%v after %d lookups, want true after 7 (3 correct in a row)", calls == 7, calls)
	}
}

func TestWaitForAddressesGivesUpWithinTheBudget(t *testing.T) {
	start := time.Now()
	lookup := func(context.Context) ([]string, error) { return []string{"140.245.11.41"}, nil }
	if waitForAddresses(context.Background(), lookup, []string{"152.67.0.60"}, 5*time.Millisecond, 60*time.Millisecond) {
		t.Fatal("wrong answers counted as published")
	}
	if time.Since(start) > time.Second {
		t.Fatal("wait overran its budget")
	}
	failing := func(context.Context) ([]string, error) { return nil, errors.New("timeout") }
	if waitForAddresses(context.Background(), failing, []string{"152.67.0.60"}, 5*time.Millisecond, 40*time.Millisecond) {
		t.Fatal("lookup errors counted as published")
	}
}
