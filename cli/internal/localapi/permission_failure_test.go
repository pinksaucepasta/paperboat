package localapi

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

type cyclicPermissionError struct{}

func (e *cyclicPermissionError) Error() string { panic("must not format private error") }
func (e *cyclicPermissionError) Unwrap() error { return e }

func TestPermissionFailureKeepsCauseAndRejectsMixedErrors(t *testing.T) {
	cause := errors.New("private transport authority proof")
	proven := PermissionFailure(cause)
	if !errors.Is(proven, ErrPermission) || !errors.Is(proven, cause) {
		t.Fatal("proven rejection lost its permission or original cause")
	}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{ErrPermission, true},
		{fmt.Errorf("authority: %w", proven), true},
		{errors.Join(proven, syscall.EIO), false},
		{errors.Join(ErrPermission, syscall.EIO), false},
		{&cyclicPermissionError{}, false},
		{(*permissionFailure)(nil), false},
	} {
		if IsPermissionFailure(tc.err) != tc.want {
			t.Fatalf("permission classification wrong for %T", tc.err)
		}
	}
}
