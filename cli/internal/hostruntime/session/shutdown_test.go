package session

import (
	"context"
	"testing"
	"time"
)

// shutdownAtEnd stops the manager's sessions and writer when the test ends, so no test leaves
// goroutines for the next repetition.
func shutdownAtEnd(t *testing.T, manager *Manager) {
	t.Helper()
	registerLeakCheck(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
}
