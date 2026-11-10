//go:build darwin || linux

package session

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestNotifyWinchWakesOnSIGWINCHUntilStopped(t *testing.T) {
	ch := make(chan os.Signal, 1)
	stop := NotifyWinch(ch)
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("a resize did not wake the receiver")
	}
	stop()
}
