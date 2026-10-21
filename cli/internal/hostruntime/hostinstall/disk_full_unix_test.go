//go:build darwin || linux

package hostinstall

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// F14-03: a full disk (or exhausted quota) while staging is a typed refusal, not a generic failure.
func TestDiskFullDetectsNoSpaceAndQuota(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT} {
		if !diskFull(fmt.Errorf("write: %w", &errorWithErrno{errno})) {
			t.Fatalf("%v not recognized as a full disk", errno)
		}
	}
	for _, other := range []error{nil, syscall.EACCES, errors.New("no space")} {
		if diskFull(other) {
			t.Fatalf("%v wrongly recognized as a full disk", other)
		}
	}
	failure := hostInstallFailure("staging installation failed: no free disk space", ErrInsufficientSpace, syscall.ENOSPC)
	if !errors.Is(failure, ErrInsufficientSpace) {
		t.Fatal("the typed refusal is not reachable from the staging failure")
	}
	if message, ok := FailureMessage(failure); !ok || message != "staging installation failed: no free disk space" {
		t.Fatalf("message = %q", message)
	}
}

type errorWithErrno struct{ errno syscall.Errno }

func (e *errorWithErrno) Error() string { return e.errno.Error() }
func (e *errorWithErrno) Unwrap() error { return e.errno }
