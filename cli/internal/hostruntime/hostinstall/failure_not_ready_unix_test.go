//go:build linux || darwin

package hostinstall

import (
	"errors"
	"syscall"
	"testing"
)

func TestNotReadyIfSuppliedNamesOnlyAFailedSuppliedActivation(t *testing.T) {
	cause := errors.New("native service did not become ready")
	if !errors.Is(notReadyIfSupplied(unixSuppliedInstall, cause), ErrUpdateNotReady) {
		t.Fatal("a supplied install whose roles did not come up was not named as rolled back")
	}
	if errors.Is(notReadyIfSupplied(unixPreparedUpdate, cause), ErrUpdateNotReady) {
		t.Fatal("a prepared (signed) update was named by the supplied-install rule")
	}
	if errors.Is(notReadyIfSupplied(unixSuppliedInstall, syscall.ENOSPC), ErrUpdateNotReady) {
		t.Fatal("a full disk lost its own message")
	}
}
