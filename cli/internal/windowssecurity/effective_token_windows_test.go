//go:build windows

package windowssecurity

import (
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

// Identity decisions follow the thread: inside WithUnelevatedToken the effective token is the thread's, not the
// (possibly elevated) process token, and outside it is the process token again.
func TestOpenEffectiveTokenFollowsTheThread(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	elevated := func() bool {
		token, err := OpenEffectiveToken()
		if err != nil {
			t.Fatal(err)
		}
		defer token.Close()
		return token.IsElevated()
	}
	process := windows.GetCurrentProcessToken().IsElevated()
	if elevated() != process {
		t.Fatalf("outside any impersonation the effective token is not the process token (process elevated=%v)", process)
	}
	if err := WithUnelevatedToken(func() error {
		if elevated() {
			t.Error("inside WithUnelevatedToken the effective token is elevated")
		}
		return nil
	}); err != nil {
		t.Skip("cannot drop the elevation here:", err)
	}
}
