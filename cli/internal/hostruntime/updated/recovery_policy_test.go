package updated

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

type recoveryPolicyBackend struct {
	recordingWindowsActivationBackend
	denied bool
}

func (b *recoveryPolicyBackend) AuthorizeRecovery(context.Context, windowsActivationJournal) error {
	if b.denied {
		return workerupdate.ErrReleaseRevoked
	}
	return nil
}

func TestWindowsRecoveryPolicyDenialPreservesServices(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationStaged, windowsActivationSwitching, windowsActivationRollingBack, windowsActivationRollbackReady} {
		t.Run(string(stage), func(t *testing.T) {
			b := &recoveryPolicyBackend{denied: true}
			journal := testWindowsActivationJournal()
			journal.Stage = stage
			_, err := executeWindowsActivation(context.Background(), b, journal)
			if !errors.Is(err, workerupdate.ErrReleaseRevoked) {
				t.Fatalf("error=%v; want recovery policy denial", err)
			}
			for _, forbidden := range []string{"candidate", "drain", "stop", "activate", "restore", "start"} {
				if slices.Contains(b.events, forbidden) {
					t.Fatalf("policy denial performed %s: %v", forbidden, b.events)
				}
			}
		})
	}
}

type canceledActivationBackend struct {
	recordingWindowsActivationBackend
	cancel context.CancelFunc
}

func (b *canceledActivationBackend) VerifyHealth(ctx context.Context, _ windowsActivationJournal) error {
	b.cancel()
	return ctx.Err()
}

func (b *canceledActivationBackend) AuthorizeRecovery(ctx context.Context, _ windowsActivationJournal) error {
	return ctx.Err()
}
func (b *canceledActivationBackend) RestoreBinary(ctx context.Context, _ windowsActivationJournal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.event("restore")
}
func TestWindowsCanceledActivationRestoresInstallation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &canceledActivationBackend{cancel: cancel}
	result, err := executeWindowsActivation(ctx, b, testWindowsActivationJournal())
	if !errors.Is(err, context.Canceled) || result.Stage != windowsActivationRolledBack || !slices.Contains(b.events, "restore") {
		t.Fatalf("stage=%s error=%v operations=%v", result.Stage, err, b.events)
	}
}
