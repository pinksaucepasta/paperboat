//go:build windows

package windowssecurity

import (
	"runtime"
	"testing"
	"unsafe"

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

func tokenIsImpersonation(t *testing.T, token windows.Token) bool {
	t.Helper()
	var kind, written uint32
	if err := windows.GetTokenInformation(token, windows.TokenType, (*byte)(unsafe.Pointer(&kind)), 4, &written); err != nil {
		t.Fatal(err)
	}
	const tokenImpersonation = 2
	return kind == tokenImpersonation
}

// The thread token is what OpenEffectiveToken returns while the thread impersonates, on any runner (no elevation needed:
// the thread impersonates its own process), and the process token again after the revert (audit C #7).
func TestOpenEffectiveTokenIsTheThreadTokenWhileImpersonating(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before, err := OpenEffectiveToken()
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	if tokenIsImpersonation(t, before) {
		t.Fatal("the effective token is an impersonation token before any impersonation")
	}
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		t.Fatal(err)
	}
	during, err := OpenEffectiveToken()
	if err != nil {
		windows.RevertToSelf()
		t.Fatal(err)
	}
	impersonating := tokenIsImpersonation(t, during)
	during.Close()
	if err := windows.RevertToSelf(); err != nil {
		t.Fatal(err)
	}
	if !impersonating {
		t.Fatal("while the thread impersonates, the effective token is not its impersonation token")
	}
	after, err := OpenEffectiveToken()
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if tokenIsImpersonation(t, after) {
		t.Fatal("the effective token is still an impersonation token after the revert")
	}
}
