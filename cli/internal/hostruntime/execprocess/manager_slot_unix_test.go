//go:build darwin || linux

package execprocess

import (
	"context"
	"testing"
	"time"
)

// A client that starts its next command as soon as Wait returns must find the slot free: 1,230 of 2,000 such
// starts were refused with ErrCapacity on macOS (TestLoadExecConcurrencyAndChurn, 2026-11-12) because the slot
// was released after the done channel closed. With the manager's lock held, Wait must not return yet.
func TestWaitReturnsOnlyAfterTheSlotIsReleased(t *testing.T) {
	manager := testManager(t, t.TempDir(), func(config *Config) { config.MaximumActive = 1; config.MaximumOperations = 16 })
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_slot_0001", Argv: []string{"/bin/echo", "ok"}, CWD: manager.config.WorkspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	manager.mu.Lock() // the slot cannot be released while the test holds this
	go func() {
		_, _ = execution.Wait(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
		manager.mu.Unlock()
		t.Fatal("Wait returned while the slot was still taken: the next start is refused with exec capacity")
	case <-time.After(500 * time.Millisecond):
	}
	manager.mu.Unlock()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait never returned after the slot was released")
	}
}
