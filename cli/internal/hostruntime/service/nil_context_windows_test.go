//go:build windows

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// A nil context used to become context.Background() on the Windows service waits (audit C #19): refused now, before
// any wait or Service Control Manager call.
func TestWindowsServiceWaitsRefuseANilContext(t *testing.T) {
	var ctx context.Context
	if err := waitWindowsServiceDeletion(ctx, "none"); !errors.Is(err, ErrLifecycleInvalid) {
		t.Errorf("waitWindowsServiceDeletion: %v", err)
	}
	probed := false
	if err := pollWindowsServiceDeletion(ctx, time.Second, time.Millisecond, func() error { probed = true; return nil }); !errors.Is(err, ErrLifecycleInvalid) || probed {
		t.Errorf("pollWindowsServiceDeletion: %v probed=%v", err, probed)
	}
	if err := stopWindowsService(ctx, nil); !errors.Is(err, ErrLifecycleInvalid) {
		t.Errorf("stopWindowsService: %v", err)
	}
	if err := waitWindowsService(ctx, nil, svc.Running); !errors.Is(err, ErrLifecycleInvalid) {
		t.Errorf("waitWindowsService: %v", err)
	}
}
