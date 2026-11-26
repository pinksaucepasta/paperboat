package daemoncmd

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

// browserTrustBudget bounds one install of the browser root into the user's stores (Linux NSS certutil writes).
const browserTrustBudget = 30 * time.Second

// browserTrustWait is how long a reconcile waits for the install before it reports browser URLs as pending.
var browserTrustWait = time.Second

var errBrowserTrustPending = errors.New("the browser certificate is being added to this user's browser stores")

// installUserTrust is a var so tests can stand in for the user's browser stores.
var installUserTrust = machineguard.InstallUserTrust

// browserTrust installs the browser root off the reconcile path. A certutil write is never cancelled midway, so a
// slow one must not use up the reconcile budget that native access shares, nor hold the coordinator's lock: the
// install runs once in the background, and browser URLs stay pending (and are retried) until it finishes.
type browserTrust struct {
	mu        sync.Mutex
	running   bool
	installed string
	failed    error
}

func (t *browserTrust) install(rootPEM []byte) error {
	key := string(rootPEM)
	t.mu.Lock()
	switch {
	case t.installed == key:
		t.mu.Unlock()
		return nil
	case t.running:
		t.mu.Unlock()
		return errBrowserTrustPending
	case t.failed != nil:
		err := t.failed
		t.failed = nil // reported once; the next retry installs again
		t.mu.Unlock()
		return err
	}
	t.running = true
	t.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), browserTrustBudget)
		err := installUserTrust(ctx, rootPEM)
		cancel()
		t.mu.Lock()
		t.running = false
		if err == nil {
			t.installed = key
		}
		t.mu.Unlock()
		done <- err
	}()
	select {
	case err := <-done: // the usual case: nothing to write, or a quick write
		return err
	case <-time.After(browserTrustWait):
		go func() { // report a slow install's failure at the next retry
			if err := <-done; err != nil {
				t.mu.Lock()
				t.failed = err
				t.mu.Unlock()
			}
		}()
		return errBrowserTrustPending
	}
}
