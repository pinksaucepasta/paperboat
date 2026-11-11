//go:build windows

package updated

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// A nil context used to become context.Background(): an activation then waited without bound on the native
// publication lock (audit C #19). Every activation path now refuses it before it waits or acts.
func TestWindowsActivationPathsRefuseANilContext(t *testing.T) {
	var ctx context.Context
	if _, err := stageWindowsActivation(ctx, WindowsConfig{}, workerupdate.Release{}); !errors.Is(err, errInvalidWindowsActivation) {
		t.Errorf("stageWindowsActivation: %v", err)
	}
	called := false
	if err := retryWindowsFileOperation(ctx, func() error { called = true; return nil }); !errors.Is(err, errInvalidWindowsActivation) || called {
		t.Errorf("retryWindowsFileOperation: %v called=%v", err, called)
	}
	if _, err := (&windowsSCMActivationBackend{}).activeHostdStatus(ctx); !errors.Is(err, errInvalidWindowsActivation) {
		t.Errorf("activeHostdStatus: %v", err)
	}
}
