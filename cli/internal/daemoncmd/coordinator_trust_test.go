package daemoncmd

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A slow browser-trust install never blocks the reconcile: it reports pending until the background install ends.
func TestBrowserTrustInstallsOffTheReconcilePath(t *testing.T) {
	release, calls := make(chan struct{}), 0
	original, wait := installUserTrust, browserTrustWait
	installUserTrust = func(context.Context, []byte) error { calls++; <-release; return nil }
	browserTrustWait = 50 * time.Millisecond
	t.Cleanup(func() { installUserTrust, browserTrustWait = original, wait })
	var trust browserTrust
	start := time.Now()
	if err := trust.install([]byte("root")); !errors.Is(err, errBrowserTrustPending) {
		t.Fatalf("first install: %v, want pending", err)
	}
	if err := trust.install([]byte("root")); !errors.Is(err, errBrowserTrustPending) {
		t.Fatalf("while running: %v, want pending", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("install blocked its caller")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for trust.install([]byte("root")) != nil {
		if time.Now().After(deadline) {
			t.Fatal("trust never became installed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls != 1 {
		t.Fatalf("installed %d times, want once", calls)
	}
}

func TestBrowserTrustReportsAQuickInstallAtOnce(t *testing.T) {
	original := installUserTrust
	failure := errors.New("no store")
	installUserTrust = func(context.Context, []byte) error { return failure }
	t.Cleanup(func() { installUserTrust = original })
	var trust browserTrust
	if err := trust.install([]byte("root")); !errors.Is(err, failure) {
		t.Fatalf("quick failing install: %v, want its error", err)
	}
}
