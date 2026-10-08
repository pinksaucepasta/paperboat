//go:build windows

package updated

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"sync"
	"testing"
)

func TestWindowsAutomaticPendingHandoffRetriesFailedLaunch(t *testing.T) {
	oldLoad, oldResume, oldDelay := loadWindowsActivationJournalForController, resumeWindowsActivationForController, windowsActivationHandoffDelay
	t.Cleanup(func() {
		loadWindowsActivationJournalForController = oldLoad
		resumeWindowsActivationForController = oldResume
		windowsActivationHandoffDelay = oldDelay
	})
	windowsActivationHandoffDelay = 0
	journal := testWindowsActivationJournal()
	journal.Stage = windowsActivationStaged
	loadWindowsActivationJournalForController = func(WindowsConfig) (windowsActivationJournal, error) { return journal, nil }
	launchFailure := errors.New("SCM unavailable")
	calls := 0
	resumeWindowsActivationForController = func(context.Context, WindowsConfig) (bool, error) {
		calls++
		if calls == 1 {
			return false, launchFailure
		}
		return true, nil
	}
	c := &windowsController{config: WindowsConfig{StateRoot: t.TempDir(), AutomaticChecks: true}, activeVersion: journal.PreviousVersion, handoff: make(chan struct{})}
	if _, err := c.automaticCheck(context.Background()); !errors.Is(err, launchFailure) || activationRequested(c.handoff) {
		t.Fatalf("failed start retired controller: %v", err)
	}
	if result, err := c.automaticCheck(context.Background()); err != nil || !activationRequested(c.handoff) || result.Version != journal.Version {
		t.Fatalf("retry did not hand off: %+v %v", result, err)
	}
	if calls != 2 {
		t.Fatalf("launch calls=%d", calls)
	}
}

func TestWindowsActivationHandoffStartsBeforeCloseExactlyOnce(t *testing.T) {
	oldDelay := windowsActivationHandoffDelay
	windowsActivationHandoffDelay = 0
	t.Cleanup(func() { windowsActivationHandoffDelay = oldDelay })
	c := &windowsController{handoff: make(chan struct{})}
	var wg sync.WaitGroup
	calls := 0
	launch := func(context.Context) error {
		calls++
		if activationRequested(c.handoff) {
			t.Error("control closed before SCM start acknowledgement")
		}
		return nil
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.handoffActivation(context.Background(), launch); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 || !activationRequested(c.handoff) {
		t.Fatalf("calls=%d handedOff=%t", calls, activationRequested(c.handoff))
	}
}

func TestWindowsActivationHandoffCancellationLeavesControllerUsable(t *testing.T) {
	c := &windowsController{handoff: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := c.handoffActivation(ctx, func(context.Context) error { calls++; return nil })
	if !errors.Is(err, context.Canceled) || calls != 0 || activationRequested(c.handoff) {
		t.Fatalf("canceled handoff: calls=%d err=%v", calls, err)
	}
}

func TestWindowsAutomaticHandoffRechecksOptOut(t *testing.T) {
	root := t.TempDir()
	settings := autoupdate.DefaultPreferences(false)
	if _, err := machineUpdateSettings(root, true, &settings); err != nil {
		t.Fatal(err)
	}
	c := &windowsController{config: WindowsConfig{StateRoot: root, AutomaticChecks: true}, handoff: make(chan struct{})}
	calls := 0
	err := c.handoffAutomaticActivation(context.Background(), func(context.Context) error { calls++; return nil })
	if err != nil || calls != 0 || activationRequested(c.handoff) {
		t.Fatalf("opt-out crossed handoff: calls=%d err=%v", calls, err)
	}
}

func TestWindowsManualPendingRetryAcknowledgesHandoff(t *testing.T) {
	oldLoad, oldResume := loadWindowsActivationJournalForController, resumeWindowsActivationForController
	t.Cleanup(func() {
		loadWindowsActivationJournalForController = oldLoad
		resumeWindowsActivationForController = oldResume
	})
	journal := testWindowsActivationJournal()
	journal.Stage = windowsActivationStaged
	journal.ApprovedCandidateID = journal.Candidate.ID
	loadWindowsActivationJournalForController = func(WindowsConfig) (windowsActivationJournal, error) { return journal, nil }
	calls := 0
	resumeWindowsActivationForController = func(context.Context, WindowsConfig) (bool, error) { calls++; return true, nil }
	scheduler, err := autoupdate.New(autoupdate.Config{Check: func(context.Context) (autoupdate.Result, error) { return autoupdate.Result{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c := &windowsController{config: WindowsConfig{StateRoot: t.TempDir()}, activeVersion: journal.PreviousVersion, scheduler: scheduler, handoff: make(chan struct{})}
	response, err := c.invoke(context.Background(), ControlRequest{Schema: ControlProtocolV1, Operation: "install", ApprovalID: journal.Candidate.ID})
	if err != nil || !response.Pending || response.Candidate == nil || response.Candidate.ID != journal.Candidate.ID || calls != 1 {
		t.Fatalf("retry lost handoff: %+v calls=%d err=%v", response, calls, err)
	}
}
