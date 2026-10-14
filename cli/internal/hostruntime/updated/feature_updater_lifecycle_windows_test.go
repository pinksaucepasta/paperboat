//go:build windows

package updated

import (
	"context"
	"errors"
	"testing"
)

func TestWindowsNativeFinalizationRestoresUpdaterWithoutRetiringPendingReceipt(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationStaged, windowsActivationSwitching, windowsActivationServicesLive, windowsActivationCommitReady} {
		restores, retires := 0, 0
		cause := errors.New("still pending")
		err := finishWindowsActivatorResult(context.Background(), windowsActivationJournal{Stage: stage}, cause, func(context.Context, windowsActivationJournal) error { restores++; return nil }, func() error { retires++; return nil })
		if !errors.Is(err, cause) || restores != 1 || retires != 0 {
			t.Fatalf("stage=%s err=%v restores=%d retires=%d", stage, err, restores, retires)
		}
	}
	restores, retires := 0, 0
	j := windowsActivationJournal{Stage: windowsActivationCommitted}
	j.Release.SupervisorMaintenance = true
	if err := finishWindowsActivatorResult(context.Background(), j, nil, func(context.Context, windowsActivationJournal) error { restores++; return nil }, func() error { retires++; return nil }); err != nil {
		t.Fatal(err)
	}
	if restores != 1 || retires != 1 {
		t.Fatalf("restores=%d retires=%d", restores, retires)
	}
}
