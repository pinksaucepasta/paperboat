//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"errors"
	"testing"
)

// Superseded roots are housekeeping on every platform: a keychain or browser database that will not
// let one go must not stop the guard from preparing and trusting the active root.
func TestPreparingTheLocalCAIgnoresAFailureToRetireOldRoots(t *testing.T) {
	previous := retireHistoricalTrust
	t.Cleanup(func() { retireHistoricalTrust = previous })
	retired := 0
	retireHistoricalTrust = func(context.Context, Config, []byte) error {
		retired++
		return errors.New("the keychain refused to remove an old root")
	}
	guard := &guardServer{cfg: Config{StateDir: t.TempDir()}}
	if err := guard.prepareLocalCA(t.Context()); err != nil {
		t.Fatalf("a failed retirement stopped the guard: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retirement was not attempted: %d", retired)
	}
}
