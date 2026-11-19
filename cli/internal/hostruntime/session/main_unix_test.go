//go:build darwin || linux

package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// A test that creates sessions and never closes them leaves their output and
// waiter goroutines behind; with -count=20 they pile up until the run times out.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// shutdownAtEnd stops the manager's sessions and writer when the test ends, so no test leaves
// goroutines for the next repetition.
var verified sync.Map

func shutdownAtEnd(t *testing.T, manager *Manager) {
	t.Helper()
	if _, seen := verified.LoadOrStore(t.Name(), true); !seen {
		t.Cleanup(func() { goleak.VerifyNone(t, goleak.IgnoreAnyFunction("database/sql.(*DB).connectionOpener")) })
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
}
