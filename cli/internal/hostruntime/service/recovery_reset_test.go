//go:build windows

package service

import "testing"

// The SCM reset period is one value for every Paperboat service; a day-long window let a few crashes over a day use up
// a recovery policy that ends in NoAction (audit C #21).
func TestWindowsRecoveryResetIsSixtySeconds(t *testing.T) {
	if WindowsRecoveryResetSeconds != 60 {
		t.Fatalf("WindowsRecoveryResetSeconds = %d, want 60", WindowsRecoveryResetSeconds)
	}
}
