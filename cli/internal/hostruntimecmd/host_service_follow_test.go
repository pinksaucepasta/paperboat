package hostruntimecmd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A Windows hostd generation's parent serves the account's host service only while the installed configuration
// names it: an update moves it to the new generation, a rollback brings it back, an unreadable configuration
// changes nothing.
func TestHostServiceFollowsTheInstalledGeneration(t *testing.T) {
	interval := hostServiceFollowInterval
	hostServiceFollowInterval = 10 * time.Millisecond
	t.Cleanup(func() { hostServiceFollowInterval = interval })
	var mu sync.Mutex
	installed, readErr, serving, starts := "old", error(nil), false, 0
	set := func(socket string, err error) { mu.Lock(); installed, readErr = socket, err; mu.Unlock() }
	state := func() (bool, int) { mu.Lock(); defer mu.Unlock(); return serving, starts }
	wait := func(want bool, wantStarts int) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if s, n := state(); s == want && n == wantStarts {
				return
			}
			if time.Now().After(deadline) {
				s, n := state()
				t.Fatalf("serving %v after %d starts, want %v after %d", s, n, want, wantStarts)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- followInstalledGeneration(ctx, "old", func() (string, error) { mu.Lock(); defer mu.Unlock(); return installed, readErr }, func(ctx context.Context) error {
			mu.Lock()
			serving, starts = true, starts+1
			mu.Unlock()
			<-ctx.Done()
			mu.Lock()
			serving = false
			mu.Unlock()
			return ctx.Err()
		})
	}()
	wait(true, 1)
	set("", errors.New("configuration being replaced"))
	time.Sleep(50 * time.Millisecond)
	wait(true, 1)
	set("new", nil)
	wait(false, 1)
	set("old", nil) // rolled back
	wait(true, 2)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("followInstalledGeneration = %v, want the cancellation", err)
	}
	if s, _ := state(); s {
		t.Fatal("still serving after ctx ended")
	}
}

// A host service that fails ends the generation's service (its parent then reports the failure) rather than
// being left down.
func TestHostServiceFailureEndsTheFollower(t *testing.T) {
	failed := errors.New("pipe failed")
	err := followInstalledGeneration(context.Background(), "g", func() (string, error) { return "g", nil }, func(context.Context) error { return failed })
	if !errors.Is(err, failed) {
		t.Fatalf("followInstalledGeneration = %v, want the server failure", err)
	}
}
