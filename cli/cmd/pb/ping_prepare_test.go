package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A prepare step that ignores its context must not leave pb ping silent forever:
// it prints a notice, then fails with a clear error within the bound.
func TestPreparePingStalledStepPrintsAndTimesOut(t *testing.T) {
	previousNotice, previousTimeout := pingPrepareNotice, pingPrepareTimeout
	pingPrepareNotice, pingPrepareTimeout = 20*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { pingPrepareNotice, pingPrepareTimeout = previousNotice, previousTimeout })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var notices bytes.Buffer
	started := time.Now()
	_, err := preparePing(context.Background(), &notices, func(context.Context) (pingPrepared, error) {
		<-release // ignores its context, like a stuck pipe dial
		return pingPrepared{}, nil
	})
	if !errors.Is(err, errPingPrepareTimeout) {
		t.Fatalf("err = %v, want errPingPrepareTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("preparePing returned after %s", elapsed)
	}
	if !strings.Contains(notices.String(), "Still preparing") {
		t.Fatalf("notices = %q, want a waiting notice", notices.String())
	}
}

func TestPreparePingFastStepIsQuiet(t *testing.T) {
	var notices bytes.Buffer
	want := errors.New("offline")
	_, err := preparePing(context.Background(), &notices, func(context.Context) (pingPrepared, error) { return pingPrepared{}, want })
	if !errors.Is(err, want) || notices.Len() != 0 {
		t.Fatalf("err = %v notices = %q", err, notices.String())
	}
}

func TestPreparePingCallerCancelWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release := make(chan struct{})
	defer close(release)
	_, err := preparePing(ctx, &bytes.Buffer{}, func(context.Context) (pingPrepared, error) { <-release; return pingPrepared{}, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
