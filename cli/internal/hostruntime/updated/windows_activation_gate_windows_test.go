//go:build windows

package updated

import (
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"testing"
	"time"
)

func TestWindowsTransactionStateMapsEveryActivationStage(t *testing.T) {
	journal := testWindowsActivationJournal()
	tests := []struct {
		stage windowsActivationStage
		want  updateflow.Stage
	}{
		{windowsActivationAwaitingApproval, updateflow.StageAwaitingApproval},
		{windowsActivationStaged, updateflow.StageStaged},
		{windowsActivationSwitching, updateflow.StageCutover},
		{windowsActivationServicesLive, updateflow.StageMonitoring},
		{windowsActivationCommitted, updateflow.StageCommitted},
		{windowsActivationRollingBack, updateflow.StageRollback},
		{windowsActivationRollbackReady, updateflow.StageRollback},
		{windowsActivationRolledBack, updateflow.StageIdle},
	}
	for _, test := range tests {
		journal.Stage = test.stage
		state := windowsTransactionState(journal)
		if state.Stage != test.want {
			t.Fatalf("stage %q mapped to %q, want %q", test.stage, state.Stage, test.want)
		}
	}
}

func TestWindowsStabilityCallTimeoutIncludesBoundedCompletionMargin(t *testing.T) {
	tests := []struct {
		window   time.Duration
		interval time.Duration
		want     time.Duration
	}{
		{10 * time.Minute, 30 * time.Second, 10*time.Minute + 30*time.Second},
		{10 * time.Minute, 250 * time.Millisecond, 10*time.Minute + time.Second},
		{10 * time.Minute, time.Minute, 10*time.Minute + 30*time.Second},
	}
	for _, test := range tests {
		if got := windowsStabilityCallTimeout(test.window, test.interval); got != test.want {
			t.Fatalf("windowsStabilityCallTimeout(%s, %s)=%s, want %s", test.window, test.interval, got, test.want)
		}
	}
}
