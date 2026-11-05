package service

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// An update journals "hostd" while hostd means the update's own generation unit,
// absent before it. A crash recovery built for the previous generation also calls
// its hostd "hostd": applying that journal entry uninstalled the live previous hostd
// (and left the candidate's unit). A journal entry applies only to the unit it describes.
func TestLifecycleRecoveryNeverAppliesAnotherGenerationsJournalEntry(t *testing.T) {
	var calls []string
	var callMu sync.Mutex
	candidate := newFakeTransactionalComponent("hostd", &calls, &callMu)
	candidate.status.Unit = "/etc/systemd/system/paperboat-hostd-u1000-candidate.service"
	manager := lifecycleForTest(t, candidate)
	before, err := candidate.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	journal := lifecycleJournal{
		Version: lifecycleJournalVersion, Operation: lifecycleInstall, StartedAt: manager.now(), Next: 1,
		Order: []string{"hostd"}, Before: []NativeComponentStatus{before},
	}
	if err = manager.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	// The crash: recovery runs with the previous generation's live hostd as "hostd".
	previous := newFakeTransactionalComponent("hostd", &calls, &callMu)
	previous.status = NativeComponentStatus{ID: "hostd", Installed: true, Enabled: true, Running: true, Ready: true,
		Definition: []byte("definition:hostd previous"), Unit: "/etc/systemd/system/paperboat-hostd-u1000-previous.service"}
	recovery, err := NewLifecycleManager(LifecycleConfig{StateRoot: manager.stateRoot, Components: []TransactionalComponent{previous}, Now: manager.now})
	if err != nil {
		t.Fatal(err)
	}
	err = recovery.Recover(context.Background())
	status, _ := previous.Inspect(context.Background())
	if !status.Installed || !status.Running {
		t.Fatalf("recovery uninstalled the live previous generation's hostd: %+v (calls %v)", status, calls)
	}
	if !errors.Is(err, ErrLifecycleUncertain) {
		t.Fatalf("a journal for another generation's unit was reported as recovered: %v", err)
	}
}
