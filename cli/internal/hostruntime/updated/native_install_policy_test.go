package updated

import (
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"testing"
)

func TestWindowsNativeInstallRetiresOnlyCompletedTransactions(t *testing.T) {
	j := testWindowsActivationJournal()
	if nativeWindowsJournalRetirable(j) {
		t.Fatal("staged update retired")
	}
	j.Stage = windowsActivationRolledBack
	if !nativeWindowsJournalRetirable(j) {
		t.Fatal("completed rollback rejected")
	}
	j.Stage = windowsActivationCommitted
	if !nativeWindowsJournalRetirable(j) {
		t.Fatal("completed update rejected")
	}
	j.Stage = windowsActivationSwitching
	if nativeWindowsJournalRetirable(j) {
		t.Fatal("activating update retired")
	}
}

func TestWindowsNativeReinstallCapturesUpdatedRollbackIdentity(t *testing.T) {
	j := testWindowsActivationJournal()
	j.Stage = windowsActivationCommitted
	got, signed, err := nativeWindowsRollbackSource(j)
	if err != nil || !signed || got.Version != j.Version || got.SHA256 != j.Runtime.SHA256 || got.Length != j.Runtime.Length {
		t.Fatalf("signed committed identity: %+v signed=%t error=%v", got, signed, err)
	}
	j.Stage = windowsActivationRolledBack
	got, signed, err = nativeWindowsRollbackSource(j)
	if err != nil || !signed || got.Version != j.PreviousVersion || got.SHA256 != j.PreviousBinary.SHA256 {
		t.Fatalf("signed previous identity: %+v signed=%t error=%v", got, signed, err)
	}
	baseline := installsource.Source{Version: j.PreviousVersion, Platform: "windows", Architecture: j.Architecture, SHA256: j.PreviousBinary.SHA256, Length: j.PreviousBinary.Length, Distribution: installsource.Official, AutomaticUpdates: true}
	j.PreviousSource = &baseline
	got, signed, err = nativeWindowsRollbackSource(j)
	if err != nil || signed || got != baseline {
		t.Fatalf("native baseline relabeled: %+v signed=%t error=%v", got, signed, err)
	}
	j.Stage = windowsActivationCommitted
	got, signed, err = nativeWindowsRollbackSource(j)
	if err != nil || !signed || got.SHA256 != j.Runtime.SHA256 {
		t.Fatalf("candidate inherited old native baseline: %+v signed=%t error=%v", got, signed, err)
	}
	for _, stage := range []windowsActivationStage{windowsActivationAwaitingApproval, windowsActivationSwitching, windowsActivationServicesLive, windowsActivationRollbackReady} {
		j.Stage = stage
		if _, _, err = nativeWindowsRollbackSource(j); err == nil {
			t.Fatalf("incomplete stage %s accepted", stage)
		}
	}
	j.Stage = windowsActivationCommitted
	j.Runtime.SHA256 = "bad"
	if _, _, err = nativeWindowsRollbackSource(j); err == nil {
		t.Fatal("corrupt authenticated identity accepted")
	}
}
